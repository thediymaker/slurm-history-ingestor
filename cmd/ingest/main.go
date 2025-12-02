package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/thediymaker/slurm-history-ingestor/internal/config"
	"github.com/thediymaker/slurm-history-ingestor/internal/ingestor"
)

func main() {
	// 1. Load Config
	cfg := config.Load()
	log.Printf("Starting Slurm Ingestor for cluster: %s", cfg.ClusterName)

	// 2. Connect to DB
	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer pool.Close()

	// 3. Initialize Ingestor
	svc, err := ingestor.New(cfg, pool)
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
