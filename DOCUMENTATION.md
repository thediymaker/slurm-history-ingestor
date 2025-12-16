# Slurm History Ingestor

## Overview
The **Slurm History Ingestor** is a standalone Go service designed to bridge the gap between a Slurm High Performance Computing (HPC) cluster and long-term analytics. It continuously ingests job history from the Slurm REST API and stores it in a PostgreSQL database.

**Key Features:**
*   **Incremental Syncing:** Automatically detects the last synced job and only fetches new data, ensuring efficiency.
*   **Robust Data Handling:** Uses a "lookback" window to catch jobs that may have finished out-of-order or during downtime.
*   **Data Normalization:** Converts complex Slurm data structures (like TRES strings and time limits) into query-friendly SQL columns (e.g., `core_hours`, `wait_time_seconds`).
*   **Multi-Cluster Support:** Can tag records with a `CLUSTER_NAME`, allowing multiple ingestors to feed into a single central database.

## Prerequisites
Before setting up the ingestor, ensure you have the following:
1.  **Slurm Cluster**: Access to a Slurm cluster with the **Slurm REST API** (slurmrestd) enabled and accessible.
    *   *Note: This tool is optimized for Slurm REST API v0.0.41.*
2.  **PostgreSQL Database**: A Postgres instance (v13+) to store the history.
3.  **Go Runtime**: Go 1.22 or higher (if building from source).

## Installation & Setup

### Quick Start (Recommended)

The easiest way to set up the ingestor is using the interactive setup script:

```bash
git clone https://github.com/thediymaker/slurm-history-ingestor.git
cd slurm-history-ingestor
chmod +x setup.sh
./setup.sh
```

The setup script will:
1. Prompt for your database connection details
2. Prompt for Slurm API configuration  
3. Auto-generate your `.env` file
4. Optionally run database migrations
5. Build the binary

### Manual Setup

If you prefer to set things up manually:

#### 1. Database Setup
The ingestor requires a specific schema to operate. Run the migration files located in `db/migrations/` against your PostgreSQL database.

```bash
# Create the database
createdb slurm_history

# Apply migrations
psql -d slurm_history -f db/migrations/001_init.sql
```

#### 2. Build the Application

```bash
git clone https://github.com/thediymaker/slurm-history-ingestor.git
cd slurm-history-ingestor

# Install sqlc and generate database code
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
sqlc generate

# Install dependencies
go mod tidy

# Build the binary
go build -o slurm-ingestor cmd/ingest/main.go
```

#### 3. Configure Environment

```bash
cp .env.example .env
# Edit .env with your values
```

## Configuration
The application is configured entirely via environment variables. You can set these in your shell, in a systemd unit file, or use a `.env` file in the working directory.

### Required Configuration

| Variable | Description | Default |
| :--- | :--- | :--- |
| `DATABASE_URL` | PostgreSQL connection string. | `postgres://user:password@localhost:5432/slurm_dashboard?sslmode=disable` |
| `SLURM_SERVER` | The base URL of your Slurm REST API. | `http://localhost:6820` |
| `SLURM_API_ACCOUNT` | The Slurm user account to authenticate as. | `slurm` |
| `SLURM_API_TOKEN` | The Slurm JWT token for authentication. | *(Empty)* |
| `CLUSTER_NAME` | A unique identifier for this cluster (useful for multi-cluster setups). | `mycluster` |

### Optional Configuration

| Variable | Description | Default |
| :--- | :--- | :--- |
| `SYNC_INTERVAL` | How often (in seconds) to check for new jobs. | `300` (5 minutes) |
| `SLURM_API_VERSION` | Slurm REST API version. | `v0.0.41` |
| `DEBUG` | Enable verbose logging for troubleshooting. | `false` |

## Storage Requirements

The job history table stores approximately **500 bytes per job record**. Plan your PostgreSQL storage accordingly:

| Jobs | Storage Required | Notes |
|------|------------------|-------|
| 100,000 | ~50 MB | Small cluster or short history |
| 1 million | ~500 MB | Medium cluster, 1-2 years |
| 10 million | ~5 GB | Large cluster or long history |
| 100 million | ~50 GB | Very large HPC environment |

**Recommendations:**
- Use a dedicated PostgreSQL server for production workloads over 1 million jobs
- Enable PostgreSQL compression (`toast.compress`) for text fields
- Consider partitioning the `job_history` table by `submit_time` for very large datasets
- Set up regular `VACUUM` and `ANALYZE` jobs for query performance

## Deployment

### Option 1: Systemd Service (Recommended)
Since the ingestor is a long-running daemon that sleeps and wakes up to sync data, the standard way to run it on Linux is using Systemd.

1.  **Install the binary:**
    ```bash
    sudo cp slurm-ingestor /usr/local/bin/
    sudo chmod +x /usr/local/bin/slurm-ingestor
    ```

2.  **Create a service file** at `/etc/systemd/system/slurm-ingestor.service`:
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
    Environment="DATABASE_URL=postgres://admin:securepass@db-host:5432/slurm_history"
    Environment="SLURM_SERVER=http://slurm-head-node:6820"
    Environment="SLURM_API_ACCOUNT=slurm_monitor"
    Environment="SLURM_API_TOKEN=your-token-here"
    Environment="CLUSTER_NAME=production-hpc"
    Environment="SYNC_INTERVAL=300"

    [Install]
    WantedBy=multi-user.target
    ```

3.  **Enable and Start:**
    ```bash
    sudo systemctl daemon-reload
    sudo systemctl enable --now slurm-ingestor
    ```

4.  **Check Status:**
    ```bash
    systemctl status slurm-ingestor
    journalctl -u slurm-ingestor -f
    ```

### Option 2: Docker (Recommended for Containerized Environments)

Docker provides an easy way to deploy the ingestor without managing dependencies.

1.  **Configure environment:**
    ```bash
    cp .env.example .env
    # Edit .env with your values
    ```

2.  **Build and run:**
    ```bash
    docker compose up -d
    ```

3.  **View logs:**
    ```bash
    docker compose logs -f ingestor
    ```

4.  **Stop:**
    ```bash
    docker compose down
    ```

**Note:** The included `docker-compose.yml` has an optional PostgreSQL service for testing. For production, comment out the postgres service and configure `DATABASE_URL` to point to your existing database.

### Option 3: Manual / Testing
You can run the binary manually for testing:
```bash
./slurm-ingestor
```

## Troubleshooting

*   **Connection Refused:** Ensure `SLURM_SERVER` is reachable and `slurmrestd` is running on the cluster.
*   **Authentication Failed:** Verify `SLURM_API_USER` exists and `SLURM_API_TOKEN` is valid and has not expired.
*   **No Jobs Syncing:** Enable `DEBUG=true`. If the logs show "No jobs found in this window," check if the `CLUSTER_NAME` matches what is in your database if you are expecting to append to existing data.
