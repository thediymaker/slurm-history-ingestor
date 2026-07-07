package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies any embedded SQL migrations that have not yet been
// recorded in the schema_migrations table.
//
// Each migration is applied inside its own transaction and its filename is
// recorded on success, so migrations are applied exactly once and in order.
// The migration SQL is written to be idempotent (IF NOT EXISTS), which makes
// this safe to adopt on databases that were migrated under the previous,
// tracking-less scheme: those objects already exist, the idempotent DDL is a
// no-op, and the version simply gets recorded.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	log.Println("Running database migrations...")

	// Ensure the tracking table exists.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("failed to create schema_migrations table: %w", err)
	}

	// Load the set of already-applied migrations.
	applied := make(map[string]bool)
	rows, err := pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("failed to read schema_migrations: %w", err)
	}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan schema_migrations row: %w", err)
		}
		applied[version] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating schema_migrations: %w", err)
	}

	// Collect and sort migration files (001_init.sql, 002_..., 003_...).
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("failed to read migrations directory: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	// Apply each pending migration in its own transaction.
	for _, filename := range files {
		if applied[filename] {
			log.Printf("  Already applied: %s", filename)
			continue
		}

		content, err := migrationsFS.ReadFile(path.Join("migrations", filename))
		if err != nil {
			return fmt.Errorf("failed to read migration %s: %w", filename, err)
		}

		log.Printf("Applying migration: %s", filename)

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("failed to begin tx for migration %s: %w", filename, err)
		}

		if _, err := tx.Exec(ctx, string(content)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("failed to apply migration %s: %w", filename, err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, filename); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("failed to record migration %s: %w", filename, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("failed to commit migration %s: %w", filename, err)
		}

		log.Printf("  Applied: %s", filename)
	}

	log.Println("Database migrations complete.")
	return nil
}
