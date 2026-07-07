# internal/db — committed, hand-customized database code

**Do not run `sqlc generate` against this package as a routine build step.**

The `.go` files here were originally produced by sqlc (v1.30.0) but have since
been **hand-customized**. Regenerating them will silently revert those edits and
break the ingestor. They are treated as **source of truth** and are committed to
the repo; the Docker image, the release workflow, and `setup.sh` all build
directly from them without regenerating.

## What was customized (and why regeneration breaks it)

- **`copyfrom.go` → `BatchInsertHistory`** — Stock sqlc emits a plain
  `CopyFrom` straight into `job_history`. That has **no conflict handling**, so
  the lookback overlap window (which re-fetches recently-finished jobs on every
  poll) produces duplicate `(job_id, cluster, submit_time)` rows and the whole
  batch fails on a primary-key violation. The committed version instead:
  1. stages the batch into a `TEMP TABLE`,
  2. de-duplicates within the batch with `DISTINCT ON`,
  3. upserts into `job_history` with `ON CONFLICT … DO UPDATE`.
  Regenerating reverts all of this and breaks incremental ingestion in API mode.

- **`models.go`** — hand-tuned types on the (currently unused) `JobHistory`
  model.

## If you genuinely need to regenerate

1. Pin the exact version: `go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0`
2. Run `sqlc generate`.
3. **Re-apply the customizations above by hand** (diff against git to see what
   sqlc reverted), then verify against a real database that API-mode ingestion
   still upserts correctly across an overlapping sync window.

`query.sql` / `query.sql.go` are kept in sync with each other so ordinary query
changes regenerate cleanly; only `copyfrom.go` and `models.go` carry edits that
sqlc cannot reproduce.
