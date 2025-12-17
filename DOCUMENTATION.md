# Slurm History Ingestor

## Overview

The **Slurm History Ingestor** is a standalone Go service that syncs job history from a Slurm HPC cluster's REST API into PostgreSQL for analytics and reporting.

**Key Features:**
- **Incremental Syncing** - Only fetches new jobs since last sync
- **Robust Data Handling** - Uses lookback window to catch out-of-order jobs
- **Data Normalization** - Converts TRES strings to query-friendly columns
- **Multi-Cluster Support** - Tag records with cluster name for centralized databases

---

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| PostgreSQL | 13+ | Database storage |
| Slurm | 20.11+ | With slurmrestd enabled (API v0.0.41) |
| Go | 1.22+ | Only if building from source |
| psql | - | PostgreSQL client for running migrations |

### Installing Required Packages

**Ubuntu/Debian:**
```bash
sudo apt update
sudo apt install -y postgresql-client
```

**RHEL/Rocky/AlmaLinux:**
```bash
sudo dnf install -y postgresql
```

**Arch Linux:**
```bash
sudo pacman -S postgresql-libs
```

*Note: You only need the PostgreSQL **client** (psql) on the machine running the ingestor. The PostgreSQL **server** can be on a different machine.*

---

## Quick Start

Choose **one** of the following installation methods:

### Option A: Setup Script (Recommended for First-Time Setup)

```bash
git clone https://github.com/thediymaker/slurm-history-ingestor.git
cd slurm-history-ingestor
chmod +x setup.sh
./setup.sh
```

The script will interactively prompt for all configuration and optionally run database migrations.

---

### Option B: Docker (Recommended for Production)

**Step 1: Clone and configure**
```bash
git clone https://github.com/thediymaker/slurm-history-ingestor.git
cd slurm-history-ingestor
cp .env.example .env
```

**Step 2: Edit `.env` with your values**
```bash
nano .env  # or use your preferred editor
```

**Step 3: Run database migrations**

You must create the database tables before starting the ingestor. Choose based on your setup:

*If using an existing PostgreSQL server:*
```bash
psql -h YOUR_DB_HOST -U YOUR_DB_USER -d YOUR_DB_NAME -f db/migrations/001_init.sql
```

*If using Docker Compose with bundled PostgreSQL (uncomment postgres service in docker-compose.yml first):*
```bash
# Start just the database first
docker compose up -d postgres

# Wait a few seconds for it to initialize, then run migrations
docker compose exec postgres psql -U slurm_user -d slurm_history -f /docker-entrypoint-initdb.d/001_init.sql
```

**Step 4: Start the ingestor**
```bash
docker compose up -d ingestor
```

**Step 5: View logs**
```bash
docker compose logs -f ingestor
```

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

**Step 2: Run database migrations**
```bash
# Download the migration file
wget https://raw.githubusercontent.com/thediymaker/slurm-history-ingestor/main/db/migrations/001_init.sql

# Apply to your database
psql -h YOUR_DB_HOST -U YOUR_DB_USER -d YOUR_DB_NAME -f 001_init.sql
```

**Step 3: Configure environment**
```bash
# Download the example config
wget https://raw.githubusercontent.com/thediymaker/slurm-history-ingestor/main/.env.example -O .env

# Edit with your values
nano .env
```

**Step 4: Run the ingestor**
```bash
./slurm-ingestor
```

---

### Option D: Build from Source

```bash
git clone https://github.com/thediymaker/slurm-history-ingestor.git
cd slurm-history-ingestor

# Install sqlc and generate database code
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
sqlc generate

# Build
go mod tidy
go build -o slurm-ingestor cmd/ingest/main.go

# Run migrations
psql -h YOUR_DB_HOST -U YOUR_DB_USER -d YOUR_DB_NAME -f db/migrations/001_init.sql

# Configure and run
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
| `DATABASE_URL` | PostgreSQL connection string | `postgres://user:pass@localhost:5432/slurm_history?sslmode=disable` |
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
| `SLURM_API_VERSION` | API version (default: `v0.0.41`) |

**Sacct Mode** (recommended) - Uses sacct command directly:
| Variable | Description |
|----------|-------------|
| `INGEST_MODE` | Set to `sacct` |
| `SACCT_PATH` | Path to sacct binary (default: `sacct`) |

> **Note:** Sacct mode is faster and more reliable. Requires running on a Slurm login node with sacct access.

### Optional

| Variable | Default | Description |
|----------|---------|-------------|
| `SYNC_INTERVAL` | `300` | Seconds between syncs |
| `INITIAL_SYNC_DATE` | `2024-01-01` | How far back to sync on first run (YYYY-MM-DD) |
| `CHUNK_HOURS` | `24` | Hours per request chunk (lower = less data per request) |
| `HTTP_TIMEOUT` | `120` | Seconds to wait for API response (API mode only) |
| `DEBUG` | `false` | Enable verbose logging |

### Performance Tuning (API Mode)

If you experience **timeout errors** with API mode, adjust these settings:

| Problem | Solution |
|---------|----------|
| "Timeout was reached" | Reduce `CHUNK_HOURS` to `6`, `3`, or even `1` |
| Slow network | Increase `HTTP_TIMEOUT` to `300` or higher |
| Very busy cluster | Switch to `INGEST_MODE=sacct` (recommended) |

The ingestor will automatically retry failed requests up to 3 times with exponential backoff.

## Running as a Systemd Service

For production deployments, run as a systemd service:

**1. Install the binary:**
```bash
sudo cp slurm-ingestor /usr/local/bin/
sudo chmod +x /usr/local/bin/slurm-ingestor
```

**2. Create `/etc/systemd/system/slurm-ingestor.service`:**
```ini
[Unit]
Description=Slurm History Ingestor
After=network.target postgresql.service

[Service]
Type=simple
User=slurm
Group=slurm
ExecStart=/usr/local/bin/slurm-ingestor
Restart=on-failure
RestartSec=10

# Configuration
Environment="DATABASE_URL=postgres://admin:pass@db-host:5432/slurm_history"
Environment="SLURM_SERVER=http://slurm-head:6820"
Environment="SLURM_API_ACCOUNT=slurm_monitor"
Environment="SLURM_API_TOKEN=your-token-here"
Environment="CLUSTER_NAME=production-hpc"

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
