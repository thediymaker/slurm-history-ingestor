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
	log.Printf("Sync interval: %ds, Lookback: %dm, Chunk: %dh, API: %s",
		i.cfg.SyncInterval, i.cfg.LookbackMinutes, i.cfg.ChunkHours, i.cfg.SlurmAPIVersion)

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
	// 1. Get last sync time
	log.Printf("Checking database for last synced job (Cluster: %s)...", i.cfg.ClusterName)
	lastTime, err := i.db.GetLastJobEndTime(ctx, i.cfg.ClusterName)
	if err != nil {
		// If error is no rows, it might be fine, but sqlc with pgx usually handles nulls via types.
		// If it's a connection error, we log it.
		// For now, we assume if it fails we start from default.
		if i.cfg.Debug {
			log.Printf("Debug: Error getting last job time (might be first run): %v", err)
		}
	}

	var startTime int64
	if lastTime.Valid {
		// Overlap window to catch jobs delayed by slurmdbd visibility or that
		// finished while the ingestor was down. Duplicates are deduped by the
		// ON CONFLICT clause on (job_id, cluster, submit_time).
		lookback := time.Duration(i.cfg.LookbackMinutes) * time.Minute
		startTime = lastTime.Time.UTC().Add(-lookback).Unix()
		log.Printf("Found last job end time: %s. Syncing from: %s (lookback: %v)",
			lastTime.Time.UTC().Format(time.RFC3339),
			time.Unix(startTime, 0).UTC().Format(time.RFC3339),
			lookback,
		)
	} else {
		// Use configured initial sync date (default: Jan 1, 2024)
		startTime = i.cfg.InitialSyncDate.UTC().Unix()
		log.Printf("No history found. Starting from configured date: %s", i.cfg.InitialSyncDate.UTC().Format("2006-01-02"))
	}

	log.Printf("Starting sync from: %s", time.Unix(startTime, 0).UTC().Format(time.RFC3339))

	endTime := time.Now().UTC().Unix()

	// Chunk by configured hours (default: 24) to avoid API timeouts
	chunkSize := int64(i.cfg.ChunkHours * 3600)

	for currentStart := startTime; currentStart < endTime; currentStart += chunkSize {
		currentEnd := currentStart + chunkSize
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

		if len(jobs) == 0 {
			if i.cfg.Debug {
				log.Println("Debug: No jobs found in this window.")
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

		// 3. Transform and Insert
		if err := i.processBatch(ctx, jobs, startTime); err != nil {
			return fmt.Errorf("batch process error: %w", err)
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

		cpusReq := int32(0)
		if job.Required != nil {
			if job.Required.CPUs != nil {
				cpusReq = *job.Required.CPUs
			} else if job.Required.CpusLower != nil {
				cpusReq = *job.Required.CpusLower
			}
		}

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
		var gpuCount int32 = 0
		if job.Tres != nil && len(job.Tres.Allocated) > 0 {
			var parts []string
			for _, t := range job.Tres.Allocated {
				label := t.Type
				if t.Name != nil {
					label = fmt.Sprintf("%s:%s", t.Type, *t.Name)
				}
				parts = append(parts, fmt.Sprintf("%s=%d", label, t.Count))

				// Extract GPU count. Slurm REST may report GPUs as either
				// type="gres", name="gpu" (or "gpu:<model>") or type="gres/gpu".
				typeLower := strings.ToLower(t.Type)
				nameLower := ""
				if t.Name != nil {
					nameLower = strings.ToLower(*t.Name)
				}
				if typeLower == "gres/gpu" || strings.HasPrefix(typeLower, "gres/gpu:") ||
					(typeLower == "gres" && (nameLower == "gpu" || strings.HasPrefix(nameLower, "gpu:"))) ||
					typeLower == "gpu" {
					gpuCount += int32(t.Count)
				}
			}
			tresAlloc = strings.Join(parts, ",")
		}

		tresReq := ""
		if job.Tres != nil && len(job.Tres.Requested) > 0 {
			var parts []string
			for _, t := range job.Tres.Requested {
				label := t.Type
				if t.Name != nil {
					label = fmt.Sprintf("%s:%s", t.Type, *t.Name)
				}
				parts = append(parts, fmt.Sprintf("%s=%d", label, t.Count))
			}
			tresReq = strings.Join(parts, ",")
		}

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

func isFinalState(state string) bool {
	// Handle states with flags like CANCELLED+
	cleanState := strings.TrimSuffix(state, "+")
	switch cleanState {
	case "COMPLETED", "FAILED", "CANCELLED", "TIMEOUT", "NODE_FAIL", "PREEMPTED", "BOOT_FAIL", "DEADLINE", "OUT_OF_MEMORY":
		return true
	}
	return false
}

// --- Raw Client Implementation ---

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
		Allocated []struct {
			Type  string  `json:"type"`
			Name  *string `json:"name"`
			Count int64   `json:"count"`
		} `json:"allocated"`
		Requested []struct {
			Type  string  `json:"type"`
			Name  *string `json:"name"`
			Count int64   `json:"count"`
		} `json:"requested"`
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
	} `json:"time"`
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
	// Assuming /slurmdb/v0.0.41/jobs
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
