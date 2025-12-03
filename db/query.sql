-- name: GetOrCreateUser :one
INSERT INTO users (name) VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING id;

-- name: GetOrCreateAccount :one
INSERT INTO accounts (name) VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING id;

-- name: GetLastJobEndTime :one
SELECT MAX(end_time)::timestamptz FROM job_history WHERE cluster = $1;

-- name: BatchInsertHistory :copyfrom
INSERT INTO job_history (
    job_id, cluster, user_id, account_id, partition, qos,
    job_state, exit_code, derived_exit_state, req_cpus, req_nodes, req_mem_mc,
    max_rss, node_list, submit_time, start_time, end_time,
    wait_time_seconds, run_time_seconds, core_hours,
    job_name, tres_alloc_str, tres_req_str,
    array_job_id, array_task_id, group_name, eligible_time, timelimit_minutes
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28
);
