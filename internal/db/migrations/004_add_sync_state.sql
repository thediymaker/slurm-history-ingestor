-- Migration: Add persisted sync cursor
-- Safe to re-run (uses IF NOT EXISTS)
--
-- One row per (cluster, ingest mode). cursor_time is the end of the last sync
-- window that was fully fetched and inserted; each pass resumes from it (minus
-- LOOKBACK_MINUTES) instead of re-deriving the start from MAX(job_history.end_time),
-- which never advances on a cluster where no new jobs finish.
--
-- Deliberately no COMMENT ON / extra indexes: existing databases may already
-- have this exact table and it must be adopted unchanged.

CREATE TABLE IF NOT EXISTS ingest_sync_state (
    cluster TEXT NOT NULL,
    mode TEXT NOT NULL, -- 'api' or 'sacct'
    cursor_time TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, mode)
);
