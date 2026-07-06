-- Migration: Add GPU utilization metrics table
-- Safe to re-run (uses IF NOT EXISTS)
--
-- This table stores historical GPU utilization metrics captured from Prometheus.
-- Data is captured by the /api/gpu endpoint (called via POST).
-- Requires: Job Metrics Plugin + GPU Utilization Plugin enabled

create table if not exists job_gpu_metrics (
  job_id text primary key,
  avg_utilization numeric(5,2) not null default 0,
  max_utilization numeric(5,2) not null default 0,
  min_utilization numeric(5,2) not null default 0,
  avg_memory_pct numeric(5,2) not null default 0,
  max_memory_pct numeric(5,2) not null default 0,
  gpu_count integer not null default 1,
  sample_count integer not null default 1,
  first_seen timestamp with time zone not null default now(),
  last_seen timestamp with time zone not null default now(),
  is_complete boolean not null default false
);

comment on table job_gpu_metrics is 'Historical GPU utilization metrics captured from Prometheus DCGM exporter.';
comment on column job_gpu_metrics.job_id is 'Slurm job ID (hpc_job label from DCGM metrics).';
comment on column job_gpu_metrics.avg_utilization is 'Running average GPU utilization percentage across all samples.';
comment on column job_gpu_metrics.max_utilization is 'Maximum GPU utilization percentage observed.';
comment on column job_gpu_metrics.min_utilization is 'Minimum GPU utilization percentage observed.';
comment on column job_gpu_metrics.avg_memory_pct is 'Running average GPU memory utilization percentage.';
comment on column job_gpu_metrics.max_memory_pct is 'Maximum GPU memory utilization percentage observed.';
comment on column job_gpu_metrics.gpu_count is 'Number of GPUs used by this job.';
comment on column job_gpu_metrics.sample_count is 'Number of samples collected (used for running average calculation).';
comment on column job_gpu_metrics.first_seen is 'Timestamp when the job was first observed.';
comment on column job_gpu_metrics.last_seen is 'Timestamp of the most recent sample.';
comment on column job_gpu_metrics.is_complete is 'True if the job has completed (no longer reporting metrics).';

create index if not exists idx_job_gpu_metrics_is_complete on job_gpu_metrics(is_complete);
create index if not exists idx_job_gpu_metrics_last_seen on job_gpu_metrics(last_seen);
