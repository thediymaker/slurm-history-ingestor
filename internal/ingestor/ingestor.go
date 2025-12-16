package ingestor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
}

func New(cfg *config.Config, pool *pgxpool.Pool) (*Ingestor, error) {
	// Create a standard HTTP client
	c := &http.Client{
		Timeout: 30 * time.Second,
	}

	return &Ingestor{
		cfg:    cfg,
		db:     db.New(pool),
		pool:   pool,
		client: c,
	}, nil
}

func (i *Ingestor) Run(ctx context.Context) error {
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
		// Use the last job's end time minus a lookback window to ensure we don't miss out-of-order jobs
		// or jobs that finished while the ingestor was down/sleeping.
		// Duplicates are handled by the database ON CONFLICT clause.
		lookback := 1 * time.Minute
		startTime = lastTime.Time.Add(-lookback).Unix()
		log.Printf("Found last job end time: %s. Syncing from: %s (lookback: %v)",
			lastTime.Time.Format(time.RFC3339),
			time.Unix(startTime, 0).Format(time.RFC3339),
			lookback,
		)
	} else {
		// Default to Jan 1, 2024 if no history found
		startTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		log.Println("No history found. Defaulting start time to Jan 1, 2024.")
	}

	log.Printf("Starting sync from: %s", time.Unix(startTime, 0).Format(time.RFC3339))

	endTime := time.Now().Unix()

	// Chunk by 7 days to speed up sync
	chunkSize := int64(7 * 24 * 3600)

	for currentStart := startTime; currentStart < endTime; currentStart += chunkSize {
		currentEnd := currentStart + chunkSize
		if currentEnd > endTime {
			currentEnd = endTime
		}

		if i.cfg.Debug {
			log.Printf("Debug: Syncing window: %d to %d", currentStart, currentEnd)
		} else {
			log.Printf("Syncing window: %s to %s", time.Unix(currentStart, 0).Format(time.RFC3339), time.Unix(currentEnd, 0).Format(time.RFC3339))
		}

		// 2. Fetch from Slurm using Raw Client to bypass strict validation
		jobs, err := i.fetchJobsRaw(ctx, currentStart, currentEnd)
		if err != nil {
			return fmt.Errorf("slurm api error: %w", err)
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
				log.Printf("Debug: First job ID: %d, Start: %s, End: %s", *j.JobId, time.Unix(s, 0).Format(time.RFC3339), time.Unix(e, 0).Format(time.RFC3339))
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
		// Get/Create Dimensions
		userID, err := i.db.GetOrCreateUser(ctx, user)
		if err != nil {
			return err
		}

		account := ""
		if job.Account != nil {
			account = *job.Account
		}
		accountID, err := i.db.GetOrCreateAccount(ctx, account)
		if err != nil {
			return err
		}

		// Transform
		// Time variables already extracted above

		startTime := time.Unix(timeStart, 0)
		endTime := time.Unix(timeEnd, 0)
		submitTime := time.Unix(timeSubmit, 0)

		runTime := timeEnd - timeStart
		waitTime := timeStart - timeSubmit

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
		if job.Tres != nil && len(job.Tres.Allocated) > 0 {
			var parts []string
			for _, t := range job.Tres.Allocated {
				label := t.Type
				if t.Name != nil {
					label = fmt.Sprintf("%s:%s", t.Type, *t.Name)
				}
				parts = append(parts, fmt.Sprintf("%s=%d", label, t.Count))
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
	// Send Local time. Slurm API v0.0.41 seems to expect Local Time strings.
	q.Set("start_time", time.Unix(start, 0).Local().Format("2006-01-02T15:04:05"))
	q.Set("end_time", time.Unix(end, 0).Local().Format("2006-01-02T15:04:05"))
	u.RawQuery = q.Encode()

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
		log.Printf("Debug: Response body: %s", string(body))
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
