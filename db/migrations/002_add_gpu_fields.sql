-- Migration: Add GPU tracking fields
-- Safe to re-run (uses IF NOT EXISTS)

-- GPU allocation tracking
ALTER TABLE job_history ADD COLUMN IF NOT EXISTS gpu_count INT DEFAULT 0;
ALTER TABLE job_history ADD COLUMN IF NOT EXISTS gpu_hours NUMERIC(12, 2) DEFAULT 0;

-- Index for GPU-focused queries
CREATE INDEX IF NOT EXISTS idx_history_gpu ON job_history (gpu_count) WHERE gpu_count > 0;
