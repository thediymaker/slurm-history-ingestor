# Slurm History Ingestor

A standalone Go service that ingests job history from a Slurm HPC cluster's REST API into PostgreSQL for analytics and reporting.

## Features

- **Incremental Syncing** - Automatically detects last synced job and only fetches new data
- **Robust Data Handling** - Uses lookback window to catch out-of-order jobs
- **Data Normalization** - Converts TRES strings to query-friendly columns (core_hours, wait_time, etc.)
- **Multi-Cluster Support** - Tag records with cluster name for centralized databases
- **Two Ingest Modes**:
  - **API Mode** - Uses Slurm REST API (slurmrestd)
  - **Sacct Mode** - Uses sacct command directly (faster, more reliable)
- **Docker Ready** - Multi-stage build for minimal production images

## Quick Start

### Option 1: Interactive Setup Script (Recommended)

```bash
chmod +x setup.sh
./setup.sh
```

The setup script will:
1. Prompt for database connection details
2. Prompt for Slurm API configuration
3. Auto-generate your `.env` file
4. Ask if you want to run database migrations
5. Build the binary

### Option 2: Docker

```bash
# Copy and configure environment
cp .env.example .env
# Edit .env with your values

# Build and run
docker compose up -d

# View logs
docker compose logs -f
```

### Option 3: Manual Setup

```bash
# 1. Clone and enter directory
git clone https://github.com/thediymaker/slurm-history-ingestor.git
cd slurm-history-ingestor

# 2. Install sqlc and generate code
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
sqlc generate

# 3. Install dependencies
go mod tidy

# 4. Run database migrations
psql -h localhost -U your_user -d slurm_history -f db/migrations/001_init.sql

# 5. Configure environment
cp .env.example .env
# Edit .env with your values

# 6. Build and run
go build -o slurm-ingestor cmd/ingest/main.go
./slurm-ingestor
```

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| Go | 1.22+ | For building from source |
| PostgreSQL | 13+ | Database storage |
| Slurm | 20.11+ | With slurmrestd (API mode) or sacct access (sacct mode) |
| sqlc | Latest | For code generation |

## Configuration

All configuration is via environment variables (or `.env` file):

### Required
| Variable | Description |
|----------|-------------|
| `DATABASE_URL` | PostgreSQL connection string |
| `CLUSTER_NAME` | Unique cluster identifier |

### Ingest Mode (choose one)
| Variable | Default | Description |
|----------|---------|-------------|
| `INGEST_MODE` | `api` | `api` (REST API) or `sacct` (direct command) |
| `SLURM_SERVER` | - | Slurm REST API URL (API mode only) |
| `SLURM_API_ACCOUNT` | - | Slurm user for API auth (API mode only) |
| `SLURM_API_TOKEN` | - | JWT token for auth (API mode only) |
| `SACCT_PATH` | `sacct` | Path to sacct binary (sacct mode only) |

### Optional
| Variable | Default | Description |
|----------|---------|-------------|
| `SYNC_INTERVAL` | `300` | Seconds between syncs |
| `INITIAL_SYNC_DATE` | `2024-01-01` | How far back to sync on first run |
| `CHUNK_HOURS` | `24` | Hours per API request (for busy clusters, try 6 or 1) |
| `DEBUG` | `false` | Enable verbose logging |

## Production Deployment

For running as a systemd service, see [DOCUMENTATION.md](DOCUMENTATION.md).

## Troubleshooting

| Issue | Solution |
|-------|----------|
| Connection Refused | Verify `SLURM_SERVER` is reachable and slurmrestd is running |
| Authentication Failed | Check `SLURM_API_ACCOUNT` exists and token is valid |
| No Jobs Syncing | Enable `DEBUG=true` and check cluster name matches |
| Migration Errors | Tables may already exist - this is usually safe to ignore |

## Architecture

```
┌─────────────────┐     ┌──────────────┐     ┌───────────────┐
│  Slurm Cluster  │────▶│   Ingestor   │────▶│  PostgreSQL   │
│  (slurmrestd)   │ API │   (Go)       │ SQL │  (History)    │
└─────────────────┘     └──────────────┘     └───────────────┘
                              │
                              ▼
                        ┌───────────┐
                        │ Dashboard │
                        │  (Next.js)│
                        └───────────┘
```

## License

MIT
