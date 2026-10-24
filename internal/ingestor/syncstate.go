package ingestor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/thediymaker/slurm-history-ingestor/internal/config"
	"github.com/thediymaker/slurm-history-ingestor/internal/db"
)

// Mode keys for ingest_sync_state.mode. API and sacct mode keep separate
// cursors so switching INGEST_MODE never trusts the other mode's progress.
const (
	syncModeAPI   = "api"
	syncModeSacct = "sacct"
)

// Where a pass's start time came from (see resolveSyncStart).
const (
	startFromCursor      = "sync cursor"
	startFromLastJobEnd  = "last job end time"
	startFromInitialDate = "INITIAL_SYNC_DATE"
)

// resolveSyncStart decides where a sync pass begins:
//   - the persisted cursor minus lookback, if a cursor exists;
//   - otherwise MAX(end_time) minus lookback (installs upgrading from before
//     the cursor existed);
//   - otherwise the configured initial sync date, with no lookback.
//
// The cursor is preferred because MAX(end_time) only moves when a new job
// finishes, so on a quiet cluster it pins every pass to the same old start and
// the re-scanned range grows without bound. Returns the start (UTC) and which
// source was used.
func resolveSyncStart(cursor, lastJobEnd pgtype.Timestamptz, initial time.Time, lookback time.Duration) (time.Time, string) {
	if cursor.Valid {
		return cursor.Time.UTC().Add(-lookback), startFromCursor
	}
	if lastJobEnd.Valid {
		return lastJobEnd.Time.UTC().Add(-lookback), startFromLastJobEnd
	}
	return initial.UTC(), startFromInitialDate
}

// loadSyncStart reads the cursor (and, if absent, the job-history fallback) for
// this cluster/mode and returns where the pass should start, logging the source.
func loadSyncStart(ctx context.Context, q *db.Queries, cfg *config.Config, mode string) (time.Time, error) {
	log.Printf("Checking database for sync cursor (Cluster: %s, Mode: %s)...", cfg.ClusterName, mode)

	cursor, err := q.GetSyncCursor(ctx, db.GetSyncCursorParams{Cluster: cfg.ClusterName, Mode: mode})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, fmt.Errorf("failed to read sync cursor: %w", err)
	}

	var lastJobEnd pgtype.Timestamptz
	if !cursor.Valid {
		lastJobEnd, err = q.GetLastJobEndTime(ctx, cfg.ClusterName)
		if err != nil {
			return time.Time{}, fmt.Errorf("failed to get last job time: %w", err)
		}
	}

	lookback := time.Duration(cfg.LookbackMinutes) * time.Minute
	start, source := resolveSyncStart(cursor, lastJobEnd, cfg.InitialSyncDate, lookback)

	switch source {
	case startFromCursor:
		log.Printf("Sync start from %s: cursor %s, syncing from %s (lookback: %v)",
			source, cursor.Time.UTC().Format(time.RFC3339), start.Format(time.RFC3339), lookback)
	case startFromLastJobEnd:
		log.Printf("Sync start from %s (no sync cursor yet): last job end %s, syncing from %s (lookback: %v)",
			source, lastJobEnd.Time.UTC().Format(time.RFC3339), start.Format(time.RFC3339), lookback)
	default:
		log.Printf("Sync start from %s (no sync cursor or job history): syncing from %s",
			source, start.Format(time.RFC3339))
	}

	return start, nil
}

// saveSyncCursor records windowEnd as the end of the last window that was
// fully fetched and inserted, so the next pass (or a restart mid-backfill)
// resumes there instead of re-scanning.
func saveSyncCursor(ctx context.Context, q *db.Queries, cfg *config.Config, mode string, windowEnd time.Time) error {
	err := q.UpsertSyncCursor(ctx, db.UpsertSyncCursorParams{
		Cluster:    cfg.ClusterName,
		Mode:       mode,
		CursorTime: pgtype.Timestamptz{Time: windowEnd.UTC(), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("failed to save sync cursor: %w", err)
	}
	if cfg.Debug {
		log.Printf("Debug: Sync cursor (mode=%s) advanced to %s", mode, windowEnd.UTC().Format(time.RFC3339))
	}
	return nil
}
