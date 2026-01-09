package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations executes all SQL migration files in order
// Safe to run multiple times - uses IF NOT EXISTS patterns
func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	log.Println("Running database migrations...")

	// Read all migration files
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("failed to read migrations directory: %w", err)
	}

	// Sort files by name (001_init.sql, 002_add_gpu_fields.sql, etc.)
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	// Execute each migration
	for _, filename := range files {
		log.Printf("Applying migration: %s", filename)

		content, err := migrationsFS.ReadFile(filepath.Join("migrations", filename))
		if err != nil {
			return fmt.Errorf("failed to read migration %s: %w", filename, err)
		}

		// Execute the migration
		_, err = pool.Exec(ctx, string(content))
		if err != nil {
			// Check if it's a "already exists" type error (expected for re-runs)
			errStr := err.Error()
			if strings.Contains(errStr, "already exists") ||
				strings.Contains(errStr, "duplicate key") {
				log.Printf("  Skipped (already applied): %s", filename)
				continue
			}
			return fmt.Errorf("failed to apply migration %s: %w", filename, err)
		}

		log.Printf("  Applied: %s", filename)
	}

	log.Println("Database migrations complete.")
	return nil
}
