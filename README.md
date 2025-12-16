# Slurm History Ingestor

A standalone Go service that ingests job history from a Slurm HPC cluster's REST API into PostgreSQL for analytics and reporting.

## Features

- **Incremental Syncing** - Automatically detects last synced job and only fetches new data
- **Robust Data Handling** - Uses lookback window to catch out-of-order jobs
- **Data Normalization** - Converts TRES strings to query-friendly columns (core_hours, wait_time, etc.)
- **Multi-Cluster Support** - Tag records with cluster name for centralized databases
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
| Slurm | 20.11+ | With slurmrestd enabled |
| sqlc | Latest | For code generation |

## Configuration

All configuration is via environment variables (or `.env` file):

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `DATABASE_URL` | Yes | - | PostgreSQL connection string |
| `SLURM_SERVER` | Yes | - | Slurm REST API URL (e.g., `http://slurm:6820`) |
| `SLURM_API_ACCOUNT` | Yes | - | Slurm user for API auth |
| `SLURM_API_TOKEN` | Yes | - | JWT token for auth |
| `CLUSTER_NAME` | Yes | - | Unique cluster identifier |
| `SLURM_API_VERSION` | No | `v0.0.41` | Slurm REST API version |
| `SYNC_INTERVAL` | No | `300` | Seconds between syncs |
| `DEBUG` | No | `false` | Enable verbose logging |

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
