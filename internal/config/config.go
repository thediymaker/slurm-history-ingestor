package config

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	DatabaseURL     string
	SlurmURL        string
	SlurmUser       string
	SlurmToken      string
	SlurmAPIVersion string
	ClusterName     string
	SyncInterval    int // Seconds
	InitialSyncDate time.Time
	ChunkHours      int    // Hours per API request chunk
	HTTPTimeout     int    // Seconds for HTTP requests
	IngestMode      string // "api" or "sacct"
	SacctPath       string // Path to sacct binary
	LookbackMinutes int    // Overlap window (minutes) to catch jobs delayed by slurmdbd visibility
	SlurmAPITZ      string // IANA timezone slurmrestd interprets zone-less timestamps in (e.g. "UTC", "America/Phoenix"). Default: UTC.
	Debug           bool
}

func Load() *Config {
	// Load .env file if it exists
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables or defaults")
	}

	return &Config{
		DatabaseURL:     getEnv("DATABASE_URL", "postgres://user:password@localhost:5432/slurm_dashboard?sslmode=require"),
		SlurmURL:        getEnv("SLURM_SERVER", "http://localhost:6820"),
		SlurmUser:       getEnv("SLURM_API_ACCOUNT", "slurm"),
		SlurmToken:      getEnv("SLURM_API_TOKEN", ""),
		SlurmAPIVersion: getEnv("SLURM_API_VERSION", "v0.0.41"),
		ClusterName:     getEnv("CLUSTER_NAME", "mycluster"),
		SyncInterval:    getEnvInt("SYNC_INTERVAL", 60),
		InitialSyncDate: getEnvDate("INITIAL_SYNC_DATE", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
		ChunkHours:      getEnvInt("CHUNK_HOURS", 24),
		HTTPTimeout:     getEnvInt("HTTP_TIMEOUT", 120),
		IngestMode:      getEnv("INGEST_MODE", "api"),
		SacctPath:       getEnv("SACCT_PATH", "sacct"),
		LookbackMinutes: getEnvInt("LOOKBACK_MINUTES", 5),
		SlurmAPITZ:      getEnv("SLURM_API_TZ", "UTC"),
		Debug:           getEnvBool("DEBUG", false),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if value, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(value); err == nil {
			return b
		}
	}
	return fallback
}

func getEnvDate(key string, fallback time.Time) time.Time {
	if value, ok := os.LookupEnv(key); ok {
		// Try parsing as YYYY-MM-DD. time.Parse uses UTC when no zone is present.
		if t, err := time.Parse("2006-01-02", value); err == nil {
			return t.UTC()
		}
		log.Printf("Warning: Invalid date format for %s: %s (expected YYYY-MM-DD)", key, value)
	}
	return fallback.UTC()
}
