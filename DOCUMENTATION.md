# Slurm History Ingestor

## Overview

The **Slurm History Ingestor** is a standalone Go service that syncs job history from a Slurm HPC cluster's REST API into PostgreSQL for analytics and reporting.

**Key Features:**
- **Incremental Syncing** - Only fetches new jobs since last sync, resuming from a persisted sync cursor (`ingest_sync_state`)
- **Robust Data Handling** - Uses lookback window to catch out-of-order jobs
- **Data Normalization** - Converts TRES strings to query-friendly columns
- **Multi-Cluster Support** - Tag records with cluster name for centralized databases

---

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| PostgreSQL | 13+ | Database storage. The ingestor creates and upgrades its tables on startup, so its user needs `CREATE` permission on the database. |
| Slurm | 20.11+ | `sacct` on the ingestor host (sacct mode), or slurmrestd (API mode). In API mode the newest API version slurmrestd serves is used unless `SLURM_API_VERSION` is set. |
| Go | 1.22+ | Only if building from source |

No PostgreSQL client is needed: the ingestor applies its migrations itself.

---

## Quick Start

Choose **one** of the following installation methods. Each one ends with the ingestor running; on first start it creates its tables and begins syncing from `INITIAL_SYNC_DATE`.

### Option A: Setup Script (Recommended for First-Time Setup)

```bash
git clone https://github.com/thediymaker/slurm-history-ingestor.git
cd slurm-history-ingestor
chmod +x setup.sh
./setup.sh
```

The script prompts for the configuration. When it offers to run database migrations you can skip that step; the ingestor applies them on startup.

---

### Option B: Docker (Recommended for Production)

Images are published to `ghcr.io/thediymaker/slurm-history-ingestor`: `:latest` and `:<version>` for releases, `:main` for the main branch. The image supports API mode; sacct mode needs `sacct` and munge on the host, so use a binary there.

```bash
# Download the example config and edit it
wget https://raw.githubusercontent.com/thediymaker/slurm-history-ingestor/main/.env.example -O .env
nano .env   # set INGEST_MODE=api, DATABASE_URL, SLURM_SERVER, SLURM_API_ACCOUNT, SLURM_API_TOKEN

docker run -d --name slurm-ingestor --restart unless-stopped --env-file .env \
  ghcr.io/thediymaker/slurm-history-ingestor:latest
docker logs -f slurm-ingestor
```

Docker's `--env-file` keeps quotes as part of the value, so write `KEY=value` without quotes.

To build the image yourself or run the bundled PostgreSQL, use `docker-compose.yaml` in the repository (`docker compose up -d`).

---

### Option C: Pre-built Binary (No Build Required)

**Step 1: Download the binary**

Download the latest release from [GitHub Releases](https://github.com/thediymaker/slurm-history-ingestor/releases).

```bash
# Example for Linux x64
wget https://github.com/thediymaker/slurm-history-ingestor/releases/latest/download/slurm-ingestor-linux-amd64
chmod +x slurm-ingestor-linux-amd64
mv slurm-ingestor-linux-amd64 slurm-ingestor
```

**Step 2: Configure environment**
```bash
# Download the example config
wget https://raw.githubusercontent.com/thediymaker/slurm-history-ingestor/main/.env.example -O .env

# Edit with your values
nano .env
```

**Step 3: Run the ingestor**
```bash
./slurm-ingestor
```

---

### Option D: Build from Source

```bash
git clone https://github.com/thediymaker/slurm-history-ingestor.git
cd slurm-history-ingestor

# NOTE: Do NOT run `sqlc generate`. The database code in internal/db is
# committed and hand-customized (see internal/db/README.md); regenerating it
# would revert the custom batch-upsert logic and break incremental ingestion.

# Build
go mod tidy
go build -o slurm-ingestor cmd/ingest/main.go

# Configure and run (migrations are applied on startup)
cp .env.example .env
nano .env
./slurm-ingestor
```

---

## Configuration Reference

All configuration is via environment variables or a `.env` file.

### Required

| Variable | Description | Example |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string | `postgres://user:pass@db-host:5432/slurm_history?sslmode=require` |
| `CLUSTER_NAME` | Unique cluster identifier | `production-hpc` |

### Ingest Mode

Choose one of two modes:

**API Mode** (default) - Uses Slurm REST API:
| Variable | Description |
|----------|-------------|
| `INGEST_MODE` | Set to `api` (default) |
| `SLURM_SERVER` | Slurm REST API URL (e.g., `http://slurm-head:6820`) |
| `SLURM_API_ACCOUNT` | Slurm user for API auth |
| `SLURM_API_TOKEN` | JWT token for auth |
| `SLURM_API_VERSION` | API version, e.g. `v0.0.42`. Unset: the newest version slurmrestd serves |
| `SLURM_API_TZ` | IANA timezone slurmrestd interprets zone-less ISO timestamps in (default: `UTC`). Must match the TZ of the slurmrestd host -- slurmrestd does **not** accept epoch integers and parses ISO timestamps in its own local timezone, not UTC. |

**Sacct Mode** (recommended) - Uses sacct command directly:
| Variable | Description |
|----------|-------------|
| `INGEST_MODE` | Set to `sacct` |
| `SACCT_PATH` | Path to sacct binary (default: `sacct`) |

> **Note:** Sacct mode is faster and more reliable. Requires running on a Slurm login node with sacct access.

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `SYNC_INTERVAL` | `60` | Seconds between syncs (near real-time) |
| `LOOKBACK_MINUTES` | `5` | Overlap window (minutes) re-scanned before the sync cursor on every sync after the first run, to absorb delayed slurmdbd visibility. Duplicates are deduped by the database `ON CONFLICT` clause. |
| `INITIAL_SYNC_DATE` | `2024-01-01` | How far back to sync on first run, i.e. when there is no sync cursor and no job history for the cluster (YYYY-MM-DD, interpreted as UTC) |
| `CHUNK_HOURS` | `24` | Hours per request chunk (lower = less data per request) |
| `CHUNK_MINUTES` | unset | Minutes per request chunk; overrides `CHUNK_HOURS`. For clusters that finish tens of thousands of jobs an hour |
| `CHUNK_TARGET_JOBS` | `0` | API mode: size each chunk to about this many finished jobs, from the count the last chunk returned: short chunks through busy stretches, long ones through quiet ones. Chunks stay between a minute and `CHUNK_HOURS`/`CHUNK_MINUTES`, which becomes the maximum |
| `BACKFILL_PAUSE_RATIO` | `0` | After each chunk of a sync pass, wait this many times as long as the chunk took. Only a pass that's behind has more than one chunk, so this paces a backfill without slowing normal syncs; `1` keeps slurmrestd busy at most half the time |
| `HTTP_TIMEOUT` | `120` | Seconds to wait for API response (API mode only) |
| `DEBUG` | `false` | Enable verbose logging |
| `TZ` | unset | Optional. The ingestor normalizes all timestamps to UTC in code; setting `TZ=UTC` on the service or container is recommended only for log clarity. |

### Timezone Handling

All time math in the ingestor is done in UTC end-to-end:

- The poll window (`startTime`, `endTime`) is computed and stored as UTC.
- **Sacct mode:** the window is formatted as `YYYY-MM-DDTHH:MM:SS` in UTC, and the `sacct` subprocess is launched with `TZ=UTC` (any inherited `TZ` is stripped first, since glibc returns the first match in `environ`). `sacct` output (`Submit`, `Start`, `End`) is parsed explicitly as UTC.
- **API mode:** slurmrestd does **not** accept epoch integers and parses zone-less ISO timestamps in its own local timezone. The window is therefore converted into the zone given by `SLURM_API_TZ` (default `UTC`) only when formatting the `start_time` / `end_time` query parameters. Set `SLURM_API_TZ` to match the TZ env of the slurmrestd / slurmctld process (e.g. `America/Phoenix`).
- `submit_time`, `start_time`, `end_time` returned from the API or sacct are converted to UTC before being written to PostgreSQL `TIMESTAMP WITH TIME ZONE` columns, which store absolute moments.

This means jobs that finish "now" are visible on the next poll (typically within `SYNC_INTERVAL + LOOKBACK_MINUTES`). If you see job history that appears to lag by hours in API mode, check that `SLURM_API_TZ` matches the slurmrestd host's local timezone.

### Performance Tuning (API Mode)

If you experience **timeout errors** with API mode, adjust these settings:

| Problem | Solution |
|---------|----------|
| "Timeout was reached" | Reduce `CHUNK_HOURS` to `6`, `3`, or even `1`, or set `CHUNK_MINUTES` |
| slurmrestd slow for everyone during a backfill | Set `CHUNK_TARGET_JOBS` (e.g. `3000`), `CHUNK_MINUTES` as the longest chunk (e.g. `30`) and `BACKFILL_PAUSE_RATIO` (e.g. `1`). slurmrestd answers no other request while it builds a large job list |
| Slow network | Increase `HTTP_TIMEOUT` to `300` or higher |
| Very busy cluster | Switch to `INGEST_MODE=sacct` (recommended) |

The ingestor will automatically retry failed requests up to 3 times with exponential backoff.

## Running as a Systemd Service

For production deployments, run as a systemd service:

**1. Install the binary and config:**
```bash
# Create installation directory
sudo mkdir -p /opt/slurm-ingestor

# Copy binary
sudo cp slurm-ingestor /opt/slurm-ingestor/
sudo chmod +x /opt/slurm-ingestor/slurm-ingestor

# Copy and edit your .env file
sudo cp .env /opt/slurm-ingestor/.env
sudo chmod 600 /opt/slurm-ingestor/.env  # Protect credentials
```

**2. Create `/etc/systemd/system/slurm-ingestor.service`:**

*Using .env file (recommended):*
```ini
[Unit]
Description=Slurm History Ingestor
After=network.target

[Service]
Type=simple
User=slurm
Group=slurm
WorkingDirectory=/opt/slurm-ingestor
ExecStart=/opt/slurm-ingestor/slurm-ingestor
Restart=on-failure
RestartSec=10

# Load configuration from .env file
EnvironmentFile=/opt/slurm-ingestor/.env

[Install]
WantedBy=multi-user.target
```

*Or define variables directly:*
```ini
[Unit]
Description=Slurm History Ingestor
After=network.target

[Service]
Type=simple
User=slurm
Group=slurm
ExecStart=/opt/slurm-ingestor/slurm-ingestor
Restart=on-failure
RestartSec=10

# Sacct mode configuration
Environment="INGEST_MODE=sacct"
Environment="DATABASE_URL=postgres://user:pass@db-host:5432/slurm_history"
Environment="CLUSTER_NAME=production-hpc"
Environment="SACCT_PATH=/usr/bin/sacct"

[Install]
WantedBy=multi-user.target
```

**3. Enable and start:**
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now slurm-ingestor
systemctl status slurm-ingestor
```

---

## What Gets Stored

Each finished job becomes one row in `job_history`: who ran it (user, account, partition, QoS), its state and exit code, what it asked for (CPUs, nodes, memory, time limit, GPUs), its submit, start and end times, and figures derived from them (wait time, run time, core hours, GPU hours).

Two columns measure what the job actually used, for the dashboard's efficiency reports:

| Column | From | Notes |
|--------|------|-------|
| `total_cpu_seconds` | sacct `TotalCPU`, or in API mode the sum of the job's steps' `time.total` (the job's own `time.total` is 0 in slurmdbd records) | User plus system CPU time summed over the job's steps, rounded to the second. CPU efficiency is `total_cpu_seconds / (run_time_seconds * req_cpus)`. NULL for jobs ingested before migration `005_add_total_cpu.sql`; NULL in API mode for jobs whose steps slurmdbd had already purged (`PurgeStepAfter`, often shorter than `PurgeJobAfter`); 0 for jobs ingested in API mode before 1.1.4. |
| `max_rss` | sacct `MaxRSS`, or in API mode the steps' `tres.requested.max` memory | Bytes; the highest per-task peak of the job's steps. NULL for jobs ingested in API mode before 1.1.4. |

In sacct mode the ingestor reads job steps as well as allocations (it no longer passes `--allocations`, with which sacct skips the steps those two figures come from). Steps are folded into their job and aren't stored as rows of their own. It also stores `AllocTRES` in `tres_alloc_str`.

---

## Storage Requirements

Approximately **500 bytes per job record**:

| Jobs | Storage |
|------|---------|
| 100,000 | ~50 MB |
| 1 million | ~500 MB |
| 10 million | ~5 GB |
| 100 million | ~50 GB |

For large deployments (>1M jobs): use a dedicated PostgreSQL server and consider table partitioning by `submit_time`.

---

## Troubleshooting

| Issue | Solution |
|-------|----------|
| Connection Refused | Verify `SLURM_SERVER` is reachable and slurmrestd is running |
| Authentication Failed | Check `SLURM_API_ACCOUNT` exists and token is valid |
| No Jobs Syncing | Enable `DEBUG=true`, verify `CLUSTER_NAME` matches |
| Migration Errors | Tables may already exist - usually safe to ignore |
