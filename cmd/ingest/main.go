package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thediymaker/slurm-history-ingestor/internal/config"
	"github.com/thediymaker/slurm-history-ingestor/internal/ingestor"
)

// Runner interface for both API and sacct ingestors
type Runner interface {
	Run(ctx context.Context) error
}

func main() {
	// 1. Load Config
	cfg := config.Load()
	log.Printf("Starting Slurm Ingestor for cluster: %s (mode: %s)", cfg.ClusterName, cfg.IngestMode)

	// 2. Connect to DB
	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer pool.Close()

	// 3. Initialize Ingestor based on mode
	var svc Runner
	mode := strings.ToLower(cfg.IngestMode)
	
	switch mode {
	case "sacct":
		log.Println("Using SACCT mode (direct sacct command)")
		svc, err = ingestor.NewSacct(cfg, pool)
	case "api", "":
		log.Println("Using API mode (Slurm REST API)")
		svc, err = ingestor.New(cfg, pool)
	default:
		log.Fatalf("Unknown INGEST_MODE: %s (valid options: api, sacct)", cfg.IngestMode)
	}
	
	if err != nil {
		log.Fatalf("Failed to initialize ingestor: %v", err)
	}

	// 4. Run with Graceful Shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		cancel()
	}()

	if err := svc.Run(ctx); err != nil {
		log.Fatalf("Ingestor stopped with error: %v", err)
	}
}

