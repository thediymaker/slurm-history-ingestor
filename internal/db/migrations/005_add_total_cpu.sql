-- Migration: Add CPU time actually used
-- Safe to re-run (uses IF NOT EXISTS)
--
-- total_cpu_seconds is user + system CPU time summed over the job's steps
-- (sacct TotalCPU, slurmdb time.total). CPU efficiency is
-- total_cpu_seconds / (run_time_seconds * req_cpus). NULL when Slurm didn't
-- report it, and for jobs ingested before this column existed.

ALTER TABLE job_history ADD COLUMN IF NOT EXISTS total_cpu_seconds BIGINT;
