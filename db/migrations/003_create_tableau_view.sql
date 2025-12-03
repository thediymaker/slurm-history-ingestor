-- Create schema for Tableau exports if it doesn't exist
CREATE SCHEMA IF NOT EXISTS tableau_export;

-- Create the view matching the expected schema
CREATE OR REPLACE VIEW tableau_export.jobs_view AS
SELECT
    j.job_id AS job_id,
    u.name AS "user",
    j.group_name AS "group",
    j.partition AS partition,
    j.qos AS qos,
    EXTRACT(EPOCH FROM j.start_time) AS start_epochtime,
    EXTRACT(EPOCH FROM j.end_time) AS end_epochtime,
    EXTRACT(EPOCH FROM j.submit_time) AS submit_epochtime,
    j.eligible_time AS eligible_epochtime,
    j.run_time_seconds AS walltime,
    j.req_cpus AS cores,
    -- Extract GPU count from TRES string (e.g., "cpu=4,mem=4G,node=1,billing=4,gres/gpu=1")
    COALESCE(substring(j.tres_alloc_str from 'gres/gpu=(\d+)')::int, 0) AS gpus,
    (j.run_time_seconds * j.req_cpus) AS coretime,
    (j.run_time_seconds * COALESCE(substring(j.tres_alloc_str from 'gres/gpu=(\d+)')::int, 0)) AS gputime,
    org_child.info AS school_code,
    org_child.name AS school_name,
    org_parent.info AS college_code,
    org_parent.name AS college_name,
    j.derived_exit_state AS exit_state
FROM job_history j
LEFT JOIN users u ON j.user_id = u.id
LEFT JOIN accounts a ON j.account_id = a.id
LEFT JOIN account_mappings am ON a.id = am.account_id
LEFT JOIN organizations org_child ON am.organization_id = org_child.id
LEFT JOIN organizations org_parent ON org_child.parent_id = org_parent.id;
