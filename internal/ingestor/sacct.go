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
}

// NewSacct creates a new sacct-based ingestor
func NewSacct(cfg *config.Config, pool *pgxpool.Pool) (*SacctIngestor, error) {
	return &SacctIngestor{
		cfg:  cfg,
		db:   db.New(pool),
		pool: pool,
	}, nil
}

// sacct output format - must match the --format string
// JobID|User|Account|Partition|State|ExitCode|Submit|Start|End|AllocCPUS|AllocNodes|NodeList|JobName|MaxRSS|TimelimitRaw|QOS|Group
const sacctFormat = "JobIDRaw,User,Account,Partition,State,ExitCode,Submit,Start,End,AllocCPUS,AllocNodes,NodeList,JobName,MaxRSS,TimelimitRaw,QOS,Group"

// SacctJob represents a parsed job from sacct output
type SacctJob struct {
	JobID       int64
	User        string
	Account     string
	Partition   string
	State       string
	ExitCode    int32
	SubmitTime  time.Time
	StartTime   time.Time
	EndTime     time.Time
	AllocCPUs   int32
	AllocNodes  int32
	NodeList    string
	JobName     string
	MaxRSS      int64
	Timelimit   int64 // in minutes
	QOS         string
	Group       string
}

// Run starts the sacct-based sync loop
func (s *SacctIngestor) Run(ctx context.Context) error {
	log.Printf("Starting Slurm History Ingestor (SACCT mode) for cluster: %s", s.cfg.ClusterName)
	log.Printf("Sync interval: %d seconds, Chunk size: %d hours", s.cfg.SyncInterval, s.cfg.ChunkHours)

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

	var startTime time.Time
	if lastTime.Valid {
		lookback := 1 * time.Minute
		startTime = lastTime.Time.Add(-lookback)
		log.Printf("Found last job end time: %s. Syncing from: %s (lookback: %v)",
			lastTime.Time.Format(time.RFC3339),
			startTime.Format(time.RFC3339),
			lookback,
		)
	} else {
		startTime = s.cfg.InitialSyncDate
		log.Printf("No history found. Starting from configured date: %s", startTime.Format("2006-01-02"))
	}

	endTime := time.Now()
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

	// Format times for sacct
	startStr := startTime.Format("2006-01-02T15:04:05")
	endStr := endTime.Format("2006-01-02T15:04:05")

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

	if s.cfg.Debug {
		log.Printf("Debug: Running: TZ=UTC %s %s", sacctPath, strings.Join(args, " "))
	}

	cmd := exec.CommandContext(ctx, sacctPath, args...)
	
	// Set TZ=UTC ensures consistent timestamps regardless of DST
	cmd.Env = append(os.Environ(), "TZ=UTC")
	
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

		// Skip jobs with invalid timestamps
		now := time.Now()
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
	if len(fields) < 17 {
		return SacctJob{}, fmt.Errorf("expected 17 fields, got %d", len(fields))
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
	}, nil
}

func parseSlurmTime(s string) time.Time {
	if s == "" || s == "Unknown" || s == "None" {
		return time.Time{}
	}
	
	// sacct outputs UTC time when TZ=UTC is set
	// Parse as UTC for consistency
	formats := []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}
	
	for _, format := range formats {
		if t, err := time.Parse(format, s); err == nil {
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
			core_hours, job_name, group_name, timelimit_minutes
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
		ON CONFLICT (job_id, cluster, submit_time) 
		DO UPDATE SET
			job_state = EXCLUDED.job_state,
			exit_code = EXCLUDED.exit_code,
			end_time = EXCLUDED.end_time,
			run_time_seconds = EXCLUDED.run_time_seconds,
			core_hours = EXCLUDED.core_hours,
			job_name = EXCLUDED.job_name
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

		// Get/create user
		userID, err := s.db.GetOrCreateUser(ctx, job.User)
		if err != nil {
			return fmt.Errorf("failed to get/create user %s: %w", job.User, err)
		}

		// Get/create account
		accountID, err := s.db.GetOrCreateAccount(ctx, job.Account)
		if err != nil {
			return fmt.Errorf("failed to get/create account %s: %w", job.Account, err)
		}

		// Calculate derived values
		runTimeSeconds := int64(job.EndTime.Sub(job.StartTime).Seconds())
		waitTimeSeconds := int64(job.StartTime.Sub(job.SubmitTime).Seconds())
		coreHours := (float64(runTimeSeconds) * float64(job.AllocCPUs)) / 3600.0

		// Execute upsert
		_, err = s.pool.Exec(ctx, upsertQuery,
			job.JobID,                 // $1
			s.cfg.ClusterName,         // $2
			userID,                    // $3
			accountID,                 // $4
			job.Partition,             // $5
			job.QOS,                   // $6
			job.State,                 // $7
			job.ExitCode,              // $8
			job.AllocCPUs,             // $9
			job.AllocNodes,            // $10
			job.MaxRSS,                // $11
			job.NodeList,              // $12
			job.SubmitTime,            // $13
			job.StartTime,             // $14
			job.EndTime,               // $15
			waitTimeSeconds,           // $16
			runTimeSeconds,            // $17
			coreHours,                 // $18
			job.JobName,               // $19
			job.Group,                 // $20
			job.Timelimit,             // $21
		)
		if err != nil {
			return fmt.Errorf("failed to upsert job %d: %w", job.JobID, err)
		}
		inserted++
	}

	log.Printf("Inserted/updated %d jobs", inserted)
	return nil
}
