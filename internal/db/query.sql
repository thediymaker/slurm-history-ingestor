-- name: GetOrCreateUser :one
-- Insert the user if new, otherwise return the existing id. Uses DO NOTHING
-- (not DO UPDATE) so repeat lookups do not write a dead row version on every
-- call -- avoiding table/index bloat and needless WAL over millions of jobs.
WITH ins AS (
    INSERT INTO users (name) VALUES ($1)
    ON CONFLICT (name) DO NOTHING
    RETURNING id
)
SELECT id FROM ins
UNION ALL
SELECT id FROM users WHERE name = $1
LIMIT 1;

-- name: GetOrCreateAccount :one
WITH ins AS (
    INSERT INTO accounts (name) VALUES ($1)
    ON CONFLICT (name) DO NOTHING
    RETURNING id
)
SELECT id FROM ins
UNION ALL
SELECT id FROM accounts WHERE name = $1
LIMIT 1;

-- name: GetLastJobEndTime :one
SELECT MAX(end_time)::timestamptz FROM job_history WHERE cluster = $1;

-- name: BatchInsertHistory :copyfrom
INSERT INTO job_history (
    job_id, cluster, user_id, account_id, partition, qos,
    job_state, exit_code, derived_exit_state, req_cpus, req_nodes, req_mem_mc,
    max_rss, node_list, submit_time, start_time, end_time,
    wait_time_seconds, run_time_seconds, core_hours,
    job_name, tres_alloc_str, tres_req_str,
    array_job_id, array_task_id, group_name, eligible_time, timelimit_minutes,
    gpu_count, gpu_hours
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30
);
