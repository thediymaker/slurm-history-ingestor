-- Dimensions
CREATE TABLE users (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE accounts (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

-- Hierarchy
CREATE TABLE organizations (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    type TEXT NOT NULL, -- 'University', 'College', 'Department'
    parent_id INT REFERENCES organizations(id),
    info TEXT, -- Extra info like 'B1343'
    CONSTRAINT unique_org_name_type UNIQUE (name, type)
);

CREATE TABLE account_mappings (
    account_id INT NOT NULL REFERENCES accounts(id),
    organization_id INT NOT NULL REFERENCES organizations(id),
    PRIMARY KEY (account_id, organization_id)
);

-- Job History
CREATE TABLE job_history (
    -- Composite Key: JobID + Cluster + SubmitTime usually ensures uniqueness
    job_id BIGINT NOT NULL,
    cluster TEXT NOT NULL,
    
    -- Dimensions (Indexed for filtering)
    user_id INT REFERENCES users(id),
    account_id INT REFERENCES accounts(id),
    partition TEXT,
    qos TEXT,
    
    -- Status & Outcomes
    job_state TEXT NOT NULL, -- 'COMPLETED', 'TIMEOUT', 'FAILED', 'CANCELLED'
    exit_code INT,
    derived_exit_state TEXT, -- 'SUCCESS', 'JOB_FAIL', 'SYSTEM_FAIL'

    -- Resources Requested vs Used
    req_cpus INT,
    req_nodes INT,
    req_mem_mc BIGINT,       -- Requested MB
    
    max_rss BIGINT,          -- Max Resident Set Size (bytes)
    node_list TEXT,          -- Compact string: "node[01-05]"

    -- Timestamps
    submit_time TIMESTAMP WITH TIME ZONE NOT NULL,
    start_time TIMESTAMP WITH TIME ZONE,
    end_time TIMESTAMP WITH TIME ZONE,

    -- Derived Analytics
    wait_time_seconds BIGINT,  -- start - submit
    run_time_seconds BIGINT,   -- end - start
    core_hours NUMERIC(12, 2), -- (run_time * req_cpus) / 3600
    
    -- Extra Fields
    job_name TEXT,
    tres_alloc_str TEXT,
    tres_req_str TEXT,

    -- Array Job Info
    array_job_id BIGINT,
    array_task_id INT,

    -- User/Group Info
    group_name TEXT,
    
    -- Additional Timestamps
    eligible_time TIMESTAMP WITH TIME ZONE,

    -- Limits
    timelimit_minutes BIGINT,

    PRIMARY KEY (job_id, cluster, submit_time)
);

-- Indices for Dashboard Reporting
CREATE INDEX idx_history_end_time ON job_history (end_time DESC);
CREATE INDEX idx_history_user ON job_history (user_id);
CREATE INDEX idx_history_account ON job_history (account_id);
CREATE INDEX idx_history_cluster ON job_history (cluster);
