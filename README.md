# Slurm History Ingestor

This is a standalone Go service to ingest Slurm job history into PostgreSQL.

## Prerequisites

- Go 1.22+
- PostgreSQL
- `sqlc` (for code generation)

## Setup

1.  **Initialize Go Module:**
    ```bash
    go mod tidy
    ```

2.  **Generate Database Code:**
    ```bash
    sqlc generate
    ```

3.  **Run Migrations:**
    Apply the SQL in `db/migrations/` to your PostgreSQL database.

4.  **Configuration:**
    Set the following environment variables:
    - `DATABASE_URL`: Postgres connection string.
    - `SLURM_SERVER`: Slurm REST API URL (e.g., `http://slurm:6820`).
    - `SLURM_API_ACCOUNT`: Slurm API user.
    - `SLURM_API_TOKEN`: Slurm API token.
    - `CLUSTER_NAME`: Name of the cluster to tag records with.

## Running

```bash
go run cmd/ingest/main.go
```
