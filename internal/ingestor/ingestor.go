package ingestor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thediymaker/slurm-history-ingestor/internal/config"
	"github.com/thediymaker/slurm-history-ingestor/internal/db"
)

type Ingestor struct {
	cfg    *config.Config
	db     *db.Queries
	pool   *pgxpool.Pool
	client *http.Client

	// In-process caches for dimension IDs. The users/accounts tables are tiny
	// and effectively append-only, so caching name->id avoids a DB round-trip
	// per job (many jobs share the same user/account). Run() is single-threaded,
	// so no locking is needed.
	userCache    map[string]int32
	accountCache map[string]int32

	// Set when slurmrestd rejects the state filter on /slurmdb/jobs (older
	// API versions may not take it); windows are then fetched unfiltered.
	noStateFilter bool
}

func New(cfg *config.Config, pool *pgxpool.Pool) (*Ingestor, error) {
	// Create a standard HTTP client
	c := &http.Client{
		Timeout: time.Duration(cfg.HTTPTimeout) * time.Second,
	}

	return &Ingestor{
		cfg:          cfg,
		db:           db.New(pool),
		pool:         pool,
		client:       c,
		userCache:    make(map[string]int32),
		accountCache: make(map[string]int32),
	}, nil
}

// getUserID returns the id for a user name, creating the row on first sight and
// caching the result for the lifetime of the process.
func (i *Ingestor) getUserID(ctx context.Context, name string) (int32, error) {
	if id, ok := i.userCache[name]; ok {
		return id, nil
	}
	id, err := i.db.GetOrCreateUser(ctx, name)
	if err != nil {
		return 0, err
	}
	i.userCache[name] = id
	return id, nil
}

// getAccountID is the account-table equivalent of getUserID.
func (i *Ingestor) getAccountID(ctx context.Context, name string) (int32, error) {
	if id, ok := i.accountCache[name]; ok {
		return id, nil
	}
	id, err := i.db.GetOrCreateAccount(ctx, name)
	if err != nil {
		return 0, err
	}
	i.accountCache[name] = id
	return id, nil
}

func (i *Ingestor) Run(ctx context.Context) error {
	log.Printf("Starting Slurm History Ingestor (API mode) for cluster: %s", i.cfg.ClusterName)
	apiVersion := i.cfg.SlurmAPIVersion
	if apiVersion == "" {
		apiVersion = "detect"
	}
	log.Printf("Sync interval: %ds, Lookback: %dm, Chunk: %s (target %d jobs), Backfill pause ratio: %g, API: %s",
		i.cfg.SyncInterval, i.cfg.LookbackMinutes, i.cfg.Chunk(), i.cfg.ChunkTargetJobs, i.cfg.BackfillPause, apiVersion)

	apiLoc, locErr := time.LoadLocation(i.cfg.SlurmAPITZ)
	if locErr != nil {
		log.Printf("Warning: invalid SLURM_API_TZ=%q, falling back to UTC: %v", i.cfg.SlurmAPITZ, locErr)
		apiLoc = time.UTC
	}
	now := time.Now()
	log.Printf("Time check: now UTC=%s | now local=%s | now slurmrestd-tz (%s)=%s",
		now.UTC().Format(time.RFC3339),
		now.Format(time.RFC3339),
		i.cfg.SlurmAPITZ,
		now.In(apiLoc).Format(time.RFC3339),
	)
	log.Printf("API query window will be formatted in TZ=%s (set SLURM_API_TZ to match slurmrestd's local timezone; default UTC).", i.cfg.SlurmAPITZ)

	// Run once immediately
	log.Println("Running initial sync...")
	if err := i.syncJobs(ctx); err != nil {
		log.Printf("Error during initial sync: %v", err)
	}

	log.Printf("Initial sync complete. Waiting %d seconds for next sync...", i.cfg.SyncInterval)

	ticker := time.NewTicker(time.Duration(i.cfg.SyncInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := i.syncJobs(ctx); err != nil {
				log.Printf("Error syncing jobs: %v", err)
			}
		}
	}
}

func (i *Ingestor) syncJobs(ctx context.Context) error {
	if err := i.ensureAPIVersion(ctx); err != nil {
		return err
	}

	// 1. Work out where this pass starts: persisted cursor, else last job end,
	// else INITIAL_SYNC_DATE (see resolveSyncStart). The lookback overlap
	// catches jobs delayed by slurmdbd visibility or that finished while the
	// ingestor was down. Duplicates are deduped by the ON CONFLICT clause on
	// (job_id, cluster, submit_time).
	start, err := loadSyncStart(ctx, i.db, i.cfg, syncModeAPI)
	if err != nil {
		return err
	}
	startTime := start.Unix()

	endTime := time.Now().UTC().Unix()

	// Chunk by CHUNK_MINUTES or CHUNK_HOURS (default: 24 hours) to avoid API
	// timeouts. With CHUNK_TARGET_JOBS each window is sized from the last one
	// instead, starting small in case the pass begins in a busy stretch.
	maxChunk := int64(i.cfg.Chunk() / time.Second)
	chunkSize := maxChunk
	if i.cfg.ChunkTargetJobs > 0 {
		chunkSize = min(maxChunk, 300)
	}

	var windowStarted time.Time
	var currentEnd int64
	for currentStart := startTime; currentStart < endTime; currentStart = currentEnd {
		if currentStart != startTime {
			if err := pauseBetweenWindows(ctx, i.cfg, time.Since(windowStarted)); err != nil {
				return err
			}
		}
		windowStarted = time.Now()
		currentEnd = currentStart + chunkSize
		if currentEnd > endTime {
			currentEnd = endTime
		}

		if i.cfg.Debug {
			log.Printf("Debug: Syncing window: %d to %d", currentStart, currentEnd)
		} else {
			log.Printf("Syncing window: %s to %s", time.Unix(currentStart, 0).UTC().Format(time.RFC3339), time.Unix(currentEnd, 0).UTC().Format(time.RFC3339))
		}

		// 2. Fetch from Slurm with retry logic for transient errors
		var jobs []RawJob
		var err error
		maxRetries := 3
		for attempt := 1; attempt <= maxRetries; attempt++ {
			jobs, err = i.fetchJobsRaw(ctx, currentStart, currentEnd)
			if err == nil {
				break
			}
			if isRetryableErr(err) && attempt < maxRetries {
				waitTime := time.Duration(attempt*attempt) * 10 * time.Second // Exponential backoff: 10s, 40s, 90s
				log.Printf("API error (attempt %d/%d): %v. Retrying in %v...", attempt, maxRetries, err, waitTime)
				// Honor cancellation during backoff so shutdown is prompt.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(waitTime):
				}
				continue
			}
			return fmt.Errorf("slurm api error after %d attempts: %w", attempt, err)
		}
		chunkSize = nextChunkSeconds(currentEnd-currentStart, maxChunk, len(jobs), i.cfg.ChunkTargetJobs)

		if len(jobs) == 0 {
			if i.cfg.Debug {
				log.Println("Debug: No jobs found in this window.")
			}
			// An empty window is still fully synced; advance past it.
			if err := saveSyncCursor(ctx, i.db, i.cfg, syncModeAPI, time.Unix(currentEnd, 0)); err != nil {
				return err
			}
			continue
		}

		if i.cfg.Debug {
			log.Printf("Debug: Found %d jobs. Processing...", len(jobs))
			if len(jobs) > 0 {
				j := jobs[0]
				s := int64(0)
				e := int64(0)
				if j.Time != nil {
					if j.Time.Start != nil {
						s = *j.Time.Start
					}
					if j.Time.End != nil {
						e = *j.Time.End
					}
				}
				log.Printf("Debug: First job ID: %d, Start: %s, End: %s", *j.JobId, time.Unix(s, 0).UTC().Format(time.RFC3339), time.Unix(e, 0).UTC().Format(time.RFC3339))
			}
		}

		// 3. Transform and Insert. filterBefore is the start of the whole pass
		// (cursor minus lookback), not this window's start.
		if err := i.processBatch(ctx, jobs, startTime); err != nil {
			return fmt.Errorf("batch process error: %w", err)
		}

		// 4. Window fully fetched and inserted: advance the cursor so the next
		// pass, or a restart mid-backfill, resumes here.
		if err := saveSyncCursor(ctx, i.db, i.cfg, syncModeAPI, time.Unix(currentEnd, 0)); err != nil {
			return err
		}
	}

	return nil
}

func (i *Ingestor) processBatch(ctx context.Context, jobs []RawJob, filterBefore int64) error {
	var params []db.BatchInsertHistoryParams

	for _, job := range jobs {
		// State
		stateStr := ""
		if job.State != nil && len(job.State.Current) > 0 {
			stateStr = job.State.Current[0]
		}

		// Filter non-final states
		if !isFinalState(stateStr) {
			if i.cfg.Debug {
				log.Printf("Debug: Skipping non-final state: %s (Job %d)", stateStr, *job.JobId)
			}
			continue
		}

		// Transform Time early for filtering
		var timeStart, timeEnd, timeSubmit int64
		if job.Time != nil {
			if job.Time.Start != nil {
				timeStart = *job.Time.Start
			}
			if job.Time.End != nil {
				timeEnd = *job.Time.End
			}
			if job.Time.Submission != nil {
				timeSubmit = *job.Time.Submission
			}
		}

		// Filter out jobs that ended before our start window (overlap duplicates)
		if timeEnd < filterBefore {
			continue
		}

		user := ""
		if job.User != nil {
			user = *job.User
		}
		// Get/Create Dimensions (cached name->id lookups)
		userID, err := i.getUserID(ctx, user)
		if err != nil {
			return err
		}

		account := ""
		if job.Account != nil {
			account = *job.Account
		}
		accountID, err := i.getAccountID(ctx, account)
		if err != nil {
			return err
		}

		// Transform
		// Time variables already extracted above

		startTime := time.Unix(timeStart, 0).UTC()
		endTime := time.Unix(timeEnd, 0).UTC()
		submitTime := time.Unix(timeSubmit, 0).UTC()

		runTime := timeEnd - timeStart
		waitTime := timeStart - timeSubmit

		// Validate timestamps - skip jobs with corrupted data
		// Some jobs have start_time far in the future (e.g., year 2106) which causes negative run_time
		now := time.Now().UTC()
		if startTime.After(now.Add(24 * time.Hour)) {
			if i.cfg.Debug {
				log.Printf("Debug: Skipping job %d with future start_time: %s", *job.JobId, startTime.Format(time.RFC3339))
			}
			continue
		}
		if runTime < 0 || waitTime < 0 {
			if i.cfg.Debug {
				log.Printf("Debug: Skipping job %d with negative runtime (%d) or waittime (%d)", *job.JobId, runTime, waitTime)
			}
			continue
		}

		cpusReq := allocatedCPUs(job)

		coreHours := (float64(runTime) * float64(cpusReq)) / 3600.0

		// Normalize Memory
		memMB := int64(0)
		if job.Required != nil {
			if job.Required.MemoryPerCpu != nil && job.Required.MemoryPerCpu.Number != nil {
				memMB = *job.Required.MemoryPerCpu.Number
			} else if job.Required.MemoryPerNode != nil && job.Required.MemoryPerNode.Number != nil {
				memMB = *job.Required.MemoryPerNode.Number
			}
		}

		var numericCoreHours pgtype.Numeric
		numericCoreHours.Scan(fmt.Sprintf("%.2f", coreHours))

		jobID := int64(0)
		if job.JobId != nil {
			jobID = *job.JobId
		}

		partition := ""
		if job.Partition != nil {
			partition = *job.Partition
		}

		qos := ""
		if job.Qos != nil {
			qos = *job.Qos
		}

		exitCode := int32(0)
		if job.ExitCode != nil && job.ExitCode.ReturnCode != nil {
			if job.ExitCode.ReturnCode.Number != nil {
				exitCode = *job.ExitCode.ReturnCode.Number
			}
		}

		nodesReq := int32(0)
		if job.AllocationNodes != nil {
			nodesReq = *job.AllocationNodes
		}

		nodeList := ""
		if job.Nodes != nil {
			nodeList = *job.Nodes
		}

		jobName := ""
		if job.Name != nil {
			jobName = *job.Name
		}

		tresAlloc := ""
		tresReq := ""
		if job.Tres != nil {
			tresAlloc = tresString(job.Tres.Allocated)
			tresReq = tresString(job.Tres.Requested)
		}
		gpuCount := allocatedGPUs(job)

		groupName := ""
		if job.Group != nil {
			groupName = *job.Group
		}

		var arrayJobIdVal pgtype.Int4
		var arrayTaskIdVal pgtype.Int4
		if job.Array != nil {
			if job.Array.JobId != nil {
				arrayJobIdVal = pgtype.Int4{Int32: *job.Array.JobId, Valid: true}
			}
			if job.Array.TaskId != nil && job.Array.TaskId.Number != nil {
				arrayTaskIdVal = pgtype.Int4{Int32: *job.Array.TaskId.Number, Valid: true}
			}
		}

		var eligibleTimeVal pgtype.Int8
		var timelimitMinutesVal pgtype.Int8
		if job.Time != nil {
			if job.Time.Eligible != nil {
				eligibleTimeVal = pgtype.Int8{Int64: *job.Time.Eligible, Valid: true}
			}
			if job.Time.Limit != nil && job.Time.Limit.Number != nil {
				timelimitMinutesVal = pgtype.Int8{Int64: *job.Time.Limit.Number, Valid: true}
			}
		}

		gpuHours := (float64(runTime) * float64(gpuCount)) / 3600.0
		var numericGpuHours pgtype.Numeric
		numericGpuHours.Scan(fmt.Sprintf("%.2f", gpuHours))

		params = append(params, db.BatchInsertHistoryParams{
			JobID:            jobID,
			Cluster:          i.cfg.ClusterName,
			UserID:           pgtype.Int4{Int32: userID, Valid: true},
			AccountID:        pgtype.Int4{Int32: accountID, Valid: true},
			Partition:        pgtype.Text{String: partition, Valid: true},
			Qos:              pgtype.Text{String: qos, Valid: true},
			JobState:         stateStr,
			ExitCode:         pgtype.Int4{Int32: exitCode, Valid: true},
			ReqCpus:          pgtype.Int4{Int32: cpusReq, Valid: true},
			ReqNodes:         pgtype.Int4{Int32: nodesReq, Valid: true},
			ReqMemMc:         pgtype.Int8{Int64: memMB, Valid: true},
			SubmitTime:       pgtype.Timestamptz{Time: submitTime, Valid: true},
			StartTime:        pgtype.Timestamptz{Time: startTime, Valid: true},
			EndTime:          pgtype.Timestamptz{Time: endTime, Valid: true},
			WaitTimeSeconds:  pgtype.Int8{Int64: waitTime, Valid: true},
			RunTimeSeconds:   pgtype.Int8{Int64: runTime, Valid: true},
			CoreHours:        numericCoreHours,
			NodeList:         pgtype.Text{String: nodeList, Valid: true},
			JobName:          pgtype.Text{String: jobName, Valid: true},
			TresAllocStr:     pgtype.Text{String: tresAlloc, Valid: true},
			TresReqStr:       pgtype.Text{String: tresReq, Valid: true},
			ArrayJobID:       arrayJobIdVal,
			ArrayTaskID:      arrayTaskIdVal,
			GroupName:        pgtype.Text{String: groupName, Valid: true},
			EligibleTime:     eligibleTimeVal,
			TimelimitMinutes: timelimitMinutesVal,
			GpuCount:         pgtype.Int4{Int32: gpuCount, Valid: true},
			GpuHours:         numericGpuHours,
			TotalCpuSeconds:  totalCPUSeconds(job),
			MaxRss:           maxRSS(job),
		})
	}

	// Bulk Insert
	// Start a transaction for the batch insert to support temp tables
	tx, err := i.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction error: %w", err)
	}
	defer tx.Rollback(ctx)

	q := i.db.WithTx(tx)

	// Note: sqlc generates a CopyFrom method on the Queries struct or similar
	// You might need to use the generated CopyFrom method directly
	count, err := q.BatchInsertHistory(ctx, params)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction error: %w", err)
	}

	log.Printf("Inserted %d jobs", count)
	return nil
}

// isRetryableErr reports whether a fetch error is worth retrying. It prefers
// typed checks and falls back to substring matching only for low-level
// connection errors that Go does not always surface as typed errors. A canceled
// context (shutdown) is deliberately NOT retryable.
func isRetryableErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Fallback for syscall-level errors (ECONNRESET/ECONNREFUSED) that arrive as
	// plain wrapped strings.
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "timeout")
}

// totalCPUSeconds is the CPU time the job used, rounded to the second: the sum
// over its steps, as sacct's TotalCPU. Without steps (slurmdbd purges them
// sooner than jobs, see PurgeStepAfter) only a nonzero job figure counts,
// since slurmdbd leaves the job's own at 0. Unset (NULL) when unknown.
func totalCPUSeconds(job RawJob) pgtype.Int8 {
	var total int64
	found := false
	if len(job.Steps) > 0 {
		for _, step := range job.Steps {
			if step.Time == nil {
				continue
			}
			if us, ok := step.Time.Total.microseconds(); ok {
				total += us
				found = true
			}
		}
	} else if job.Time != nil {
		total, found = job.Time.Total.microseconds()
		found = found && total > 0
	}
	if !found {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: (total + 500_000) / 1_000_000, Valid: true}
}

// maxRSS is the job's peak memory in bytes: the highest per-task peak over its
// steps, as sacct mode records it. Unset when no step reported memory.
func maxRSS(job RawJob) pgtype.Int8 {
	var peak int64
	found := false
	for _, step := range job.Steps {
		if step.Tres == nil || step.Tres.Requested == nil {
			continue
		}
		for _, m := range step.Tres.Requested.Max {
			if m.Type == "mem" {
				found = true
				peak = max(peak, m.Count)
			}
		}
	}
	if !found {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: peak, Valid: true}
}

// tresString lists resources as sacct's AllocTRES and ReqTRES do:
// "cpu=8,mem=81920,node=1,gres/gpu=1,gres/gpu:h100=1"
func tresString(tres []RawTres) string {
	parts := make([]string, 0, len(tres))
	for _, t := range tres {
		parts = append(parts, fmt.Sprintf("%s=%d", t.label(), t.Count))
	}
	return strings.Join(parts, ",")
}

// allocatedCPUs is the CPUs the job held, as sacct's AllocCPUS: the cpu entry
// of its allocated TRES. A whole-node or exclusive job holds more than it
// asked for. Without one, the CPUs it asked for.
func allocatedCPUs(job RawJob) int32 {
	if job.Tres != nil {
		for _, t := range job.Tres.Allocated {
			if t.Type == "cpu" {
				return int32(t.Count)
			}
		}
	}
	if job.Required != nil {
		if job.Required.CPUs != nil {
			return *job.Required.CPUs
		}
		if job.Required.CpusLower != nil {
			return *job.Required.CpusLower
		}
	}
	return 0
}

// allocatedGPUs is the GPUs the job held. Slurm lists them once as gres/gpu
// and again by model (gres/gpu:a100), so the generic entry counts when there
// is one, else the sum of the typed ones.
func allocatedGPUs(job RawJob) int32 {
	if job.Tres == nil {
		return 0
	}
	var generic, typed int64
	hasGeneric := false
	for _, t := range job.Tres.Allocated {
		switch label := strings.ToLower(t.label()); {
		case label == "gres/gpu" || label == "gpu":
			generic += t.Count
			hasGeneric = true
		case strings.HasPrefix(label, "gres/gpu:"):
			typed += t.Count
		}
	}
	if hasGeneric {
		return int32(generic)
	}
	return int32(typed)
}

// nextChunkSeconds sizes the next API window from how many jobs the last one
// (cur seconds long) returned, aiming for about target jobs: busy stretches get
// short windows, each a small slurmrestd response, and quiet stretches long
// ones, so a backfill needs fewer requests. The window stays between a minute
// and maxChunk, and grows at most fourfold at a time so a quiet stretch
// doesn't run straight into the middle of a busy one. target 0 keeps windows
// at maxChunk.
func nextChunkSeconds(cur, maxChunk int64, jobs, target int) int64 {
	if target <= 0 {
		return maxChunk
	}
	next := cur * 4
	if jobs > 0 {
		next = min(next, cur*int64(target)/int64(jobs))
	}
	return max(min(next, maxChunk), 60)
}

// pauseBetweenWindows waits BACKFILL_PAUSE_RATIO times as long as the last
// window took, before the next window of the same sync pass. Only a pass that
// is behind has more than one window, so this paces a backfill: a ratio of 1
// keeps slurmrestd and slurmdbd busy at most half the time during heavy
// stretches, and costs almost nothing where windows are quick.
func pauseBetweenWindows(ctx context.Context, cfg *config.Config, lastWindow time.Duration) error {
	pause := time.Duration(cfg.BackfillPause * float64(lastWindow))
	if pause <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pause):
		return nil
	}
}

// The job states the ingestor records. Every other state is still running or
// waiting and is picked up once it reaches one of these.
var finalStates = []string{"COMPLETED", "FAILED", "CANCELLED", "TIMEOUT", "NODE_FAIL", "PREEMPTED", "BOOT_FAIL", "DEADLINE", "OUT_OF_MEMORY"}

func isFinalState(state string) bool {
	// Handle states with flags like CANCELLED+
	cleanState := strings.TrimSuffix(state, "+")
	for _, s := range finalStates {
		if cleanState == s {
			return true
		}
	}
	return false
}

// --- Raw Client Implementation ---

type rawDuration struct {
	Seconds      *int64 `json:"seconds"`
	Microseconds *int64 `json:"microseconds"`
}

// microseconds is the duration in microseconds; ok is false when unset
func (d *rawDuration) microseconds() (us int64, ok bool) {
	if d == nil || d.Seconds == nil {
		return 0, false
	}
	us = *d.Seconds * 1_000_000
	if d.Microseconds != nil {
		us += *d.Microseconds
	}
	return us, true
}

// A job step: CPU time it used and its highest per-task usage (tres.requested.max,
// where mem is peak RSS in bytes)
type RawStep struct {
	Time *struct {
		Total *rawDuration `json:"total"`
	} `json:"time"`
	Tres *struct {
		Requested *struct {
			Max []struct {
				Type  string `json:"type"`
				Count int64  `json:"count"`
			} `json:"max"`
		} `json:"requested"`
	} `json:"tres"`
}

// One trackable resource: type "cpu", "mem" (MB), "node", or type "gres" with
// a name such as "gpu" or "gpu:a100"
type RawTres struct {
	Type  string  `json:"type"`
	Name  *string `json:"name"`
	Count int64   `json:"count"`
}

// label is the resource as Slurm prints it: "cpu", or "gres/gpu:a100" when it
// has a name
func (t RawTres) label() string {
	if t.Name == nil || *t.Name == "" {
		return t.Type
	}
	return t.Type + "/" + *t.Name
}

type RawJobResponse struct {
	Jobs []RawJob `json:"jobs"`
}

type RawJob struct {
	JobId     *int64  `json:"job_id"`
	Cluster   *string `json:"cluster"`
	User      *string `json:"user"`
	Group     *string `json:"group"`
	Account   *string `json:"account"`
	Partition *string `json:"partition"`
	Qos       *string `json:"qos"`
	Nodes     *string `json:"nodes"` // NodeList
	Name      *string `json:"name"`  // JobName
	Array     *struct {
		JobId  *int32 `json:"job_id"`
		TaskId *struct {
			Number *int32 `json:"number"`
		} `json:"task_id"`
	} `json:"array"`
	Tres *struct {
		Allocated []RawTres `json:"allocated"`
		Requested []RawTres `json:"requested"`
	} `json:"tres"`
	State *struct {
		Current []string `json:"current"`
	} `json:"state"`
	Time *struct {
		Start      *int64 `json:"start"`
		End        *int64 `json:"end"`
		Submission *int64 `json:"submission"`
		Eligible   *int64 `json:"eligible"`
		Limit      *struct {
			Number *int64 `json:"number"`
		} `json:"limit"`
		// User + system CPU time. slurmdbd leaves the job's own figure at 0
		// and keeps the real ones on its steps
		Total *rawDuration `json:"total"`
	} `json:"time"`
	Steps    []RawStep `json:"steps"`
	Required *struct {
		CPUs         *int32 `json:"CPUs"`
		CpusLower    *int32 `json:"cpus"`
		MemoryPerCpu *struct {
			Number *int64 `json:"number"`
		} `json:"memory_per_cpu"`
		MemoryPerNode *struct {
			Number *int64 `json:"number"`
		} `json:"memory_per_node"`
	} `json:"required"`
	ExitCode *struct {
		ReturnCode *struct {
			Number *int32 `json:"number"`
		} `json:"return_code"`
	} `json:"exit_code"`
	AllocationNodes *int32 `json:"allocation_nodes"`
}

func (i *Ingestor) fetchJobsRaw(ctx context.Context, start, end int64) ([]RawJob, error) {
	// Construct URL
	// /slurmdb/<SLURM_API_VERSION>/jobs
	// We need to respect the scheme and host from config

	// Get base URL from config
	baseURL := i.cfg.SlurmURL
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	// Append endpoint
	endpoint := baseURL + "slurmdb/" + i.cfg.SlurmAPIVersion + "/jobs"

	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}

	q := u.Query()
	// slurmrestd parses zone-less ISO timestamps in its OWN local timezone
	// (not UTC, and not the ingestor's TZ). Epoch integers are rejected by
	// data_parser/v0.0.40+. We therefore format the wall-clock string in the
	// timezone slurmrestd is running in, configured via SLURM_API_TZ (default
	// "UTC"). If your ingestor receives jobs shifted by a fixed offset, that
	// env var is wrong.
	apiLoc, locErr := time.LoadLocation(i.cfg.SlurmAPITZ)
	if locErr != nil {
		log.Printf("Warning: invalid SLURM_API_TZ=%q, falling back to UTC: %v", i.cfg.SlurmAPITZ, locErr)
		apiLoc = time.UTC
	}
	startStr := time.Unix(start, 0).In(apiLoc).Format("2006-01-02T15:04:05")
	endStr := time.Unix(end, 0).In(apiLoc).Format("2006-01-02T15:04:05")
	q.Set("start_time", startStr)
	q.Set("end_time", endStr)
	// Without a state, slurmdbd returns every job that was pending or running
	// at any point in the window, so on a busy cluster most of each response
	// is jobs that end later and get skipped. With terminal states it returns
	// the jobs that ended in the window. slurmrestd takes one state per
	// parameter; a comma-separated list is rejected.
	filtered := !i.noStateFilter
	if filtered {
		for _, s := range finalStates {
			q.Add("state", s)
		}
	}
	u.RawQuery = q.Encode()

	if i.cfg.Debug {
		log.Printf("Debug: API query: start_time=%s end_time=%s (slurmrestd TZ=%s, now UTC=%s)",
			startStr, endStr, apiLoc.String(),
			time.Now().UTC().Format(time.RFC3339),
		)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request error: %w", err)
	}

	// Add headers
	if i.cfg.SlurmUser != "" {
		req.Header.Set("X-SLURM-USER-NAME", i.cfg.SlurmUser)
	}
	if i.cfg.SlurmToken != "" {
		req.Header.Set("X-SLURM-USER-TOKEN", i.cfg.SlurmToken)
	}

	if i.cfg.Debug {
		log.Printf("Debug: Fetching raw jobs from %s", u.String())
	}

	resp, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if filtered && strings.Contains(string(body), "state") {
			preview := string(body)
			if len(preview) > 300 {
				preview = preview[:300]
			}
			log.Printf("slurmrestd rejected the job state filter (status %d: %s); fetching every job in each window from now on", resp.StatusCode, preview)
			i.noStateFilter = true
			return i.fetchJobsRaw(ctx, start, end)
		}
		return nil, fmt.Errorf("slurm api returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body error: %w", err)
	}

	if i.cfg.Debug {
		// Truncate: the full response can be many MB of job records (usernames,
		// job names, node lists) and floods logs / aggregators.
		const maxLog = 1000
		preview := string(body)
		if len(preview) > maxLog {
			preview = preview[:maxLog] + fmt.Sprintf("... (%d bytes total, truncated)", len(body))
		}
		log.Printf("Debug: Response body (truncated to %d bytes): %s", maxLog, preview)
	}

	var jobResp RawJobResponse
	if err := json.Unmarshal(body, &jobResp); err != nil {
		// If decode fails, it might be empty or malformed
		// Check if it's the "no jobs" case which might return empty object?
		// But usually it returns { "jobs": [] } or { "jobs": null }
		return nil, fmt.Errorf("json decode error: %w", err)
	}

	return jobResp.Jobs, nil
}
