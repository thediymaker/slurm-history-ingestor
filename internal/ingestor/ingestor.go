package ingestor

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thediymaker/slurm-history-ingestor/internal/config"
	"github.com/thediymaker/slurm-history-ingestor/internal/db"
	slurm "github.com/ubccr/slurmrest"
)

type Ingestor struct {
	cfg    *config.Config
	db     *db.Queries
	pool   *pgxpool.Pool
	client slurm.SlurmClient
}

func New(cfg *config.Config, pool *pgxpool.Pool) (*Ingestor, error) {
	// Initialize Slurm Client
	// Note: The actual initialization depends on the library version.
	// This is a placeholder for the client setup.
	c, err := slurm.NewClient(slurm.ClientConfig{
		Url:      cfg.SlurmURL,
		Username: cfg.SlurmUser,
		Token:    cfg.SlurmToken,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create slurm client: %w", err)
	}

	return &Ingestor{
		cfg:    cfg,
		db:     db.New(pool),
		pool:   pool,
		client: c,
	}, nil
}

func (i *Ingestor) Run(ctx context.Context) error {
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
	lastTime, err := i.db.GetLastJobEndTime(ctx, i.cfg.ClusterName)
	if err != nil {
		// If no rows, default to 30 days ago or some start time
		log.Printf("No previous history found, starting from 30 days ago")
		// Handle null/error appropriately
	}

	var startTime int64
	if lastTime.Valid {
		startTime = lastTime.Time.Unix()
	} else {
		startTime = time.Now().Add(-30 * 24 * time.Hour).Unix()
	}

	endTime := time.Now().Unix()

	// Chunk by 1 hour to avoid timeouts
	chunkSize := int64(3600)

	for currentStart := startTime; currentStart < endTime; currentStart += chunkSize {
		currentEnd := currentStart + chunkSize
		if currentEnd > endTime {
			currentEnd = endTime
		}

		log.Printf("Syncing window: %d to %d", currentStart, currentEnd)

		// 2. Fetch from Slurm
		// Note: Adjust method name based on actual library
		jobs, err := i.client.DbGetJobs(ctx, &slurm.DbGetJobsOptions{
			TimeStart: currentStart,
			TimeEnd:   currentEnd,
		})
		if err != nil {
			return fmt.Errorf("slurm api error: %w", err)
		}

		if len(jobs) == 0 {
			continue
		}

		// 3. Transform and Insert
		if err := i.processBatch(ctx, jobs); err != nil {
			return fmt.Errorf("batch process error: %w", err)
		}
	}

	return nil
}

func (i *Ingestor) processBatch(ctx context.Context, jobs []slurm.DbJob) error {
	var params []db.BatchInsertHistoryParams

	for _, job := range jobs {
		// Filter non-final states
		if !isFinalState(job.State) {
			continue
		}

		// Get/Create Dimensions
		userID, err := i.db.GetOrCreateUser(ctx, job.User)
		if err != nil {
			return err
		}

		accountID, err := i.db.GetOrCreateAccount(ctx, job.Account)
		if err != nil {
			return err
		}

		// Transform
		startTime := time.Unix(job.TimeStart, 0)
		endTime := time.Unix(job.TimeEnd, 0)
		submitTime := time.Unix(job.TimeSubmit, 0)

		runTime := job.TimeEnd - job.TimeStart
		waitTime := job.TimeStart - job.TimeSubmit
		coreHours := (float64(runTime) * float64(job.CpusReq)) / 3600.0

		// Normalize Memory
		memMB := parseMemory(job.MemoryReq)

		var numericCoreHours pgtype.Numeric
		numericCoreHours.Scan(fmt.Sprintf("%.2f", coreHours))

		params = append(params, db.BatchInsertHistoryParams{
			JobID:           int64(job.JobId),
			Cluster:         i.cfg.ClusterName,
			UserID:          pgtype.Int4{Int32: userID, Valid: true},
			AccountID:       pgtype.Int4{Int32: accountID, Valid: true},
			Partition:       pgtype.Text{String: job.Partition, Valid: true},
			Qos:             pgtype.Text{String: job.Qos, Valid: true},
			JobState:        job.State,
			ExitCode:        pgtype.Int4{Int32: int32(job.ExitCode), Valid: true},
			ReqCpus:         pgtype.Int4{Int32: int32(job.CpusReq), Valid: true},
			ReqNodes:        pgtype.Int4{Int32: int32(job.NodesReq), Valid: true},
			ReqMemMc:        pgtype.Int8{Int64: memMB, Valid: true},
			SubmitTime:      pgtype.Timestamptz{Time: submitTime, Valid: true},
			StartTime:       pgtype.Timestamptz{Time: startTime, Valid: true},
			EndTime:         pgtype.Timestamptz{Time: endTime, Valid: true},
			WaitTimeSeconds: pgtype.Int8{Int64: waitTime, Valid: true},
			RunTimeSeconds:  pgtype.Int8{Int64: runTime, Valid: true},
			CoreHours:       numericCoreHours,
		})
	}

	// Bulk Insert
	// Note: sqlc generates a CopyFrom method on the Queries struct or similar
	// You might need to use the generated CopyFrom method directly
	count, err := i.db.BatchInsertHistory(ctx, params)
	if err != nil {
		return err
	}

	log.Printf("Inserted %d jobs", count)
	return nil
}

func isFinalState(state string) bool {
	switch state {
	case "COMPLETED", "FAILED", "CANCELLED", "TIMEOUT", "NODE_FAIL":
		return true
	}
	return false
}

func parseMemory(memStr string) int64 {
	if memStr == "" {
		return 0
	}
	// Remove 'Mc' suffix if present (common in Slurm DB)
	memStr = strings.TrimSuffix(memStr, "Mc")
	memStr = strings.TrimSpace(memStr)

	if len(memStr) == 0 {
		return 0
	}

	// Check last character for unit
	last := memStr[len(memStr)-1]
	var multiplier int64 = 1

	// If it ends in a letter, determine multiplier and strip it
	if last >= 'A' && last <= 'z' {
		switch last {
		case 'K', 'k':
			// Slurm base is usually MB. 1K is 1/1024 MB.
			// For simplicity, we might just treat as 0 or 1 if < 1MB,
			// but let's assume we want MB.
			return 0
		case 'M', 'm':
			multiplier = 1
		case 'G', 'g':
			multiplier = 1024
		case 'T', 't':
			multiplier = 1024 * 1024
		}
		memStr = memStr[:len(memStr)-1]
	}

	val, err := strconv.ParseInt(memStr, 10, 64)
	if err != nil {
		return 0
	}
	return val * multiplier
}
