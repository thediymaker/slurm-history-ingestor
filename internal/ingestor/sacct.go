package ingestor

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/thediymaker/slurm-history-ingestor/internal/config"
	"github.com/thediymaker/slurm-history-ingestor/internal/db"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SacctIngestor uses the sacct command directly instead of REST API
type SacctIngestor struct {
	cfg  *config.Config
	db   *db.Queries
	pool *pgxpool.Pool

	// In-process name->id caches for dimension tables (see Ingestor for
	// rationale). Run() is single-threaded, so no locking is needed.
	userCache    map[string]int32
	accountCache map[string]int32
}

// NewSacct creates a new sacct-based ingestor
func NewSacct(cfg *config.Config, pool *pgxpool.Pool) (*SacctIngestor, error) {
	return &SacctIngestor{
		cfg:          cfg,
		db:           db.New(pool),
		pool:         pool,
		userCache:    make(map[string]int32),
		accountCache: make(map[string]int32),
	}, nil
}

// getUserID returns the id for a user name, creating and caching on first sight.
func (s *SacctIngestor) getUserID(ctx context.Context, name string) (int32, error) {
	if id, ok := s.userCache[name]; ok {
		return id, nil
	}
	id, err := s.db.GetOrCreateUser(ctx, name)
	if err != nil {
		return 0, err
	}
	s.userCache[name] = id
	return id, nil
}

// getAccountID is the account-table equivalent of getUserID.
func (s *SacctIngestor) getAccountID(ctx context.Context, name string) (int32, error) {
	if id, ok := s.accountCache[name]; ok {
		return id, nil
	}
	id, err := s.db.GetOrCreateAccount(ctx, name)
	if err != nil {
		return 0, err
	}
	s.accountCache[name] = id
	return id, nil
}

// sacct output format - must match the --format string
// JobID|User|Account|Partition|State|ExitCode|Submit|Start|End|AllocCPUS|AllocNodes|NodeList|JobName|MaxRSS|TimelimitRaw|QOS|Group|AllocTRES
const sacctFormat = "JobIDRaw,User,Account,Partition,State,ExitCode,Submit,Start,End,AllocCPUS,AllocNodes,NodeList,JobName,MaxRSS,TimelimitRaw,QOS,Group,AllocTRES"

// SacctJob represents a parsed job from sacct output
type SacctJob struct {
	JobID      int64
	User       string
	Account    string
	Partition  string
	State      string
	ExitCode   int32
	SubmitTime time.Time
	StartTime  time.Time
	EndTime    time.Time
	AllocCPUs  int32
	AllocNodes int32
	NodeList   string
	JobName    string
	MaxRSS     int64
	Timelimit  int64 // in minutes
	QOS        string
	Group      string
	GpuCount   int32 // Parsed from AllocTRES
}

// Run starts the sacct-based sync loop
func (s *SacctIngestor) Run(ctx context.Context) error {
	log.Printf("Starting Slurm History Ingestor (SACCT mode) for cluster: %s", s.cfg.ClusterName)
	log.Printf("Sync interval: %ds, Lookback: %dm, Chunk: %dh",
		s.cfg.SyncInterval, s.cfg.LookbackMinutes, s.cfg.ChunkHours)
	hostTZ := os.Getenv("TZ")
	if hostTZ == "" {
		hostTZ = "<unset, host default>"
	}
	log.Printf("Time check: now UTC=%s, now local=%s, host TZ env=%s, sacct will run with TZ=UTC",
		time.Now().UTC().Format(time.RFC3339),
		time.Now().Format(time.RFC3339),
		hostTZ,
	)

	ticker := time.NewTicker(time.Duration(s.cfg.SyncInterval) * time.Second)
	defer ticker.Stop()

	// Run immediately on start
	if err := s.sync(ctx); err != nil {
		log.Printf("Error syncing jobs: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down sacct ingestor...")
			return nil
		case <-ticker.C:
			if err := s.sync(ctx); err != nil {
				log.Printf("Error syncing jobs: %v", err)
			}
		}
	}
}

func (s *SacctIngestor) sync(ctx context.Context) error {
	log.Printf("Checking database for last synced job (Cluster: %s)...", s.cfg.ClusterName)

	// Get last job end time
	lastTime, err := s.db.GetLastJobEndTime(ctx, s.cfg.ClusterName)
	if err != nil {
		return fmt.Errorf("failed to get last job time: %w", err)
	}

	// UTC is a hard invariant: all window boundaries are normalized to UTC before being
	// formatted for sacct or used in comparisons. Do not rely on the subprocess TZ env
	// for correctness; the timestamps must already be UTC by the time sacct sees them.
	var startTime time.Time
	if lastTime.Valid {
		lookback := time.Duration(s.cfg.LookbackMinutes) * time.Minute
		startTime = lastTime.Time.UTC().Add(-lookback)
		log.Printf("Found last job end time: %s. Syncing from: %s (lookback: %v)",
			lastTime.Time.UTC().Format(time.RFC3339),
			startTime.Format(time.RFC3339),
			lookback,
		)
	} else {
		startTime = s.cfg.InitialSyncDate.UTC()
		log.Printf("No history found. Starting from configured date: %s (UTC)", startTime.Format("2006-01-02"))
	}

	endTime := time.Now().UTC()
	chunkDuration := time.Duration(s.cfg.ChunkHours) * time.Hour

	for currentStart := startTime; currentStart.Before(endTime); currentStart = currentStart.Add(chunkDuration) {
		currentEnd := currentStart.Add(chunkDuration)
		if currentEnd.After(endTime) {
			currentEnd = endTime
		}

		log.Printf("Syncing window: %s to %s", currentStart.Format(time.RFC3339), currentEnd.Format(time.RFC3339))

		// Fetch jobs using sacct
		jobs, err := s.fetchJobs(ctx, currentStart, currentEnd)
		if err != nil {
			return fmt.Errorf("sacct error: %w", err)
		}

		if len(jobs) == 0 {
			if s.cfg.Debug {
				log.Println("Debug: No jobs found in this window.")
			}
			continue
		}

		log.Printf("Found %d jobs in this window", len(jobs))

		// Process and insert jobs
		if err := s.processJobs(ctx, jobs); err != nil {
			return fmt.Errorf("failed to process jobs: %w", err)
		}
	}

	return nil
}

func (s *SacctIngestor) fetchJobs(ctx context.Context, startTime, endTime time.Time) ([]SacctJob, error) {
	sacctPath := s.cfg.SacctPath
	if sacctPath == "" {
		sacctPath = "sacct"
	}

	// Format times for sacct. We force UTC here so the wall-clock string we pass
	// matches the TZ=UTC environment we hand the subprocess below. Without this,
	// time.Time values that are still in local zone get formatted as local
	// wall-clock and then reinterpreted by sacct as UTC, shifting the entire
	// poll window by the local UTC offset.
	startStr := startTime.UTC().Format("2006-01-02T15:04:05")
	endStr := endTime.UTC().Format("2006-01-02T15:04:05")

	args := []string{
		"--allusers",
		"--parsable2",
		"--noheader",
		"--allocations", // Only job allocations, not steps (.batch, .extern)
		"--duplicates",  // Include array job duplicates
		"--clusters", s.cfg.ClusterName,
		"--format", sacctFormat,
		"--starttime", startStr,
		"--endtime", endStr,
	}

	cmd := exec.CommandContext(ctx, sacctPath, args...)

	// IMPORTANT: glibc's getenv() returns the FIRST match in environ. If the
	// host already has TZ set (e.g. TZ=America/Phoenix on a US cluster), simply
	// appending "TZ=UTC" does NOT override it -- sacct still runs in local time
	// and the wall-clock strings we pass (formatted as UTC above) are then
	// reinterpreted by sacct as local time, shifting the poll window by the
	// local UTC offset. We must strip any pre-existing TZ from the inherited
	// environment before appending our UTC value.
	cmd.Env = appendTZ(os.Environ(), "UTC")

	// Always log the actual window we're about to ask sacct for, alongside the
	// current UTC time. If "now UTC" is not between starttime and endtime + a
	// few seconds of slack, the ingestor is misconfigured.
	log.Printf("sacct query: starttime=%s endtime=%s (now UTC=%s, TZ=UTC)",
		startStr, endStr, time.Now().UTC().Format("2006-01-02T15:04:05"))

	if s.cfg.Debug {
		log.Printf("Debug: Running: TZ=UTC %s %s", sacctPath, strings.Join(args, " "))
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	// Capture stderr for error messages
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start sacct: %w", err)
	}

	var jobs []SacctJob
	scanner := bufio.NewScanner(stdout)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		if line == "" {
			continue
		}

		job, err := s.parseSacctLine(line)
		if err != nil {
			if s.cfg.Debug {
				log.Printf("Debug: Skipping line %d: %v", lineNum, err)
			}
			continue
		}

		// Skip jobs with invalid timestamps (compare in UTC for consistency).
		now := time.Now().UTC()
		if job.StartTime.After(now.Add(24 * time.Hour)) {
			if s.cfg.Debug {
				log.Printf("Debug: Skipping job %d with future start_time: %s", job.JobID, job.StartTime)
			}
			continue
		}

		runTime := job.EndTime.Sub(job.StartTime)
		waitTime := job.StartTime.Sub(job.SubmitTime)
		if runTime < 0 || waitTime < 0 {
			if s.cfg.Debug {
				log.Printf("Debug: Skipping job %d with negative runtime or waittime", job.JobID)
			}
			continue
		}

		jobs = append(jobs, job)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading sacct output: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		stderrMsg := stderrBuf.String()
		if stderrMsg != "" {
			return nil, fmt.Errorf("sacct command failed: %s", strings.TrimSpace(stderrMsg))
		}
		return nil, fmt.Errorf("sacct command failed: %w", err)
	}

	return jobs, nil
}

func (s *SacctIngestor) parseSacctLine(line string) (SacctJob, error) {
	fields := strings.Split(line, "|")
	if len(fields) < 18 {
		return SacctJob{}, fmt.Errorf("expected 18 fields, got %d", len(fields))
	}

	// Parse JobID (handle array jobs like "12345_0")
	jobIDStr := fields[0]
	// Remove array suffix for base job ID
	if idx := strings.Index(jobIDStr, "_"); idx != -1 {
		jobIDStr = jobIDStr[:idx]
	}
	// Remove .batch or .extern suffix
	if idx := strings.Index(jobIDStr, "."); idx != -1 {
		jobIDStr = jobIDStr[:idx]
	}

	jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
	if err != nil {
		return SacctJob{}, fmt.Errorf("invalid job ID %q: %w", fields[0], err)
	}

	// Parse exit code (format: "0:0" or just "0")
	exitCode := int32(0)
	exitCodeStr := fields[5]
	if idx := strings.Index(exitCodeStr, ":"); idx != -1 {
		exitCodeStr = exitCodeStr[:idx]
	}
	if exitCodeStr != "" {
		if ec, err := strconv.ParseInt(exitCodeStr, 10, 32); err == nil {
			exitCode = int32(ec)
		}
	}

	// Parse timestamps
	submitTime := parseSlurmTime(fields[6])
	startTime := parseSlurmTime(fields[7])
	endTime := parseSlurmTime(fields[8])

	// Parse numeric fields
	allocCPUs := parseInt32(fields[9])
	allocNodes := parseInt32(fields[10])

	// Parse MaxRSS (can be in K, M, G format)
	maxRSS := parseMemory(fields[13])

	// Parse timelimit (in minutes, can be "UNLIMITED")
	timelimit := int64(0)
	if fields[14] != "" && fields[14] != "UNLIMITED" {
		if tl, err := strconv.ParseInt(fields[14], 10, 64); err == nil {
			timelimit = tl
		}
	}

	return SacctJob{
		JobID:      jobID,
		User:       fields[1],
		Account:    fields[2],
		Partition:  fields[3],
		State:      fields[4],
		ExitCode:   exitCode,
		SubmitTime: submitTime,
		StartTime:  startTime,
		EndTime:    endTime,
		AllocCPUs:  allocCPUs,
		AllocNodes: allocNodes,
		NodeList:   fields[11],
		JobName:    fields[12],
		MaxRSS:     maxRSS,
		Timelimit:  timelimit,
		QOS:        fields[15],
		Group:      fields[16],
		GpuCount:   parseGpuCount(fields[17]),
	}, nil
}

// appendTZ returns a copy of env with any existing TZ= entry removed and
// "TZ=<value>" appended. This is necessary because glibc's getenv() returns
// the first match in environ, so simply appending a duplicate TZ would not
// override an inherited value (e.g. TZ=America/Phoenix on a US cluster).
func appendTZ(env []string, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "TZ=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "TZ="+value)
	return out
}

func parseSlurmTime(s string) time.Time {
	if s == "" || s == "Unknown" || s == "None" {
		return time.Time{}
	}

	// sacct outputs zone-less wall-clock; we run the subprocess with TZ=UTC, so
	// the values are UTC. Use ParseInLocation with time.UTC to make that explicit
	// rather than relying on time.Parse's implicit UTC default.
	formats := []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}

	for _, format := range formats {
		if t, err := time.ParseInLocation(format, s, time.UTC); err == nil {
			return t
		}
	}

	return time.Time{}
}

func parseInt32(s string) int32 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(s, 10, 32)
	return int32(v)
}

func parseMemory(s string) int64 {
	if s == "" {
		return 0
	}

	s = strings.TrimSpace(s)
	multiplier := int64(1)

	if strings.HasSuffix(s, "K") {
		multiplier = 1024
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "M") {
		multiplier = 1024 * 1024
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "G") {
		multiplier = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	}

	v, _ := strconv.ParseFloat(s, 64)
	return int64(v * float64(multiplier))
}

// parseGpuCount extracts GPU count from AllocTRES string
// Format: "billing=8,cpu=8,gres/gpu=4,mem=64G,node=1"
func parseGpuCount(tres string) int32 {
	if tres == "" {
		return 0
	}

	// Split by comma and look for gres/gpu
	parts := strings.Split(tres, ",")
	for _, part := range parts {
		// Look for "gres/gpu=" or "gres/gpu:"
		if strings.Contains(part, "gres/gpu") {
			// Extract the number after = or :
			idx := strings.Index(part, "=")
			if idx == -1 {
				idx = strings.Index(part, ":")
			}
			if idx != -1 && idx < len(part)-1 {
				numStr := part[idx+1:]
				// Handle cases like "gres/gpu=4" or "gres/gpu:a100=4"
				if colonIdx := strings.LastIndex(numStr, ":"); colonIdx != -1 {
					numStr = numStr[colonIdx+1:]
				}
				if v, err := strconv.ParseInt(numStr, 10, 32); err == nil {
					return int32(v)
				}
			}
		}
	}
	return 0
}

func (s *SacctIngestor) processJobs(ctx context.Context, jobs []SacctJob) error {
	// Deduplicate jobs - sacct can return duplicates
	// Key: job_id + cluster + submit_time
	seen := make(map[string]bool)
	inserted := 0

	// Upsert query - insert or update each job
	upsertQuery := `
		INSERT INTO job_history (
			job_id, cluster, user_id, account_id, partition, qos,
			job_state, exit_code, req_cpus, req_nodes, max_rss, node_list,
			submit_time, start_time, end_time, wait_time_seconds, run_time_seconds,
			core_hours, job_name, group_name, timelimit_minutes, gpu_count, gpu_hours
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
		ON CONFLICT (job_id, cluster, submit_time) 
		DO UPDATE SET
			job_state = EXCLUDED.job_state,
			exit_code = EXCLUDED.exit_code,
			end_time = EXCLUDED.end_time,
			run_time_seconds = EXCLUDED.run_time_seconds,
			core_hours = EXCLUDED.core_hours,
			job_name = EXCLUDED.job_name,
			gpu_count = EXCLUDED.gpu_count,
			gpu_hours = EXCLUDED.gpu_hours
	`

	for _, job := range jobs {
		// Skip jobs that haven't ended
		if job.EndTime.IsZero() || job.StartTime.IsZero() {
			continue
		}

		// Create unique key for deduplication
		key := fmt.Sprintf("%d|%s|%d", job.JobID, s.cfg.ClusterName, job.SubmitTime.Unix())
		if seen[key] {
			continue
		}
		seen[key] = true

		// Get/create user (cached name->id lookup)
		userID, err := s.getUserID(ctx, job.User)
		if err != nil {
			return fmt.Errorf("failed to get/create user %s: %w", job.User, err)
		}

		// Get/create account (cached name->id lookup)
		accountID, err := s.getAccountID(ctx, job.Account)
		if err != nil {
			return fmt.Errorf("failed to get/create account %s: %w", job.Account, err)
		}

		// Calculate derived values
		runTimeSeconds := int64(job.EndTime.Sub(job.StartTime).Seconds())
		waitTimeSeconds := int64(job.StartTime.Sub(job.SubmitTime).Seconds())
		coreHours := (float64(runTimeSeconds) * float64(job.AllocCPUs)) / 3600.0
		gpuHours := (float64(runTimeSeconds) * float64(job.GpuCount)) / 3600.0

		// Execute upsert
		_, err = s.pool.Exec(ctx, upsertQuery,
			job.JobID,         // $1
			s.cfg.ClusterName, // $2
			userID,            // $3
			accountID,         // $4
			job.Partition,     // $5
			job.QOS,           // $6
			job.State,         // $7
			job.ExitCode,      // $8
			job.AllocCPUs,     // $9
			job.AllocNodes,    // $10
			job.MaxRSS,        // $11
			job.NodeList,      // $12
			job.SubmitTime,    // $13
			job.StartTime,     // $14
			job.EndTime,       // $15
			waitTimeSeconds,   // $16
			runTimeSeconds,    // $17
			coreHours,         // $18
			job.JobName,       // $19
			job.Group,         // $20
			job.Timelimit,     // $21
			job.GpuCount,      // $22
			gpuHours,          // $23
		)
		if err != nil {
			return fmt.Errorf("failed to upsert job %d: %w", job.JobID, err)
		}
		inserted++
	}

	log.Printf("Inserted/updated %d jobs", inserted)
	return nil
}
