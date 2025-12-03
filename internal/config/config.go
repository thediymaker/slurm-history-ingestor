package config

import (
	"log"
	"os"
	"strconv"

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
	Debug           bool
}

func Load() *Config {
	// Load .env file if it exists
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables or defaults")
	}

	return &Config{
		DatabaseURL:     getEnv("DATABASE_URL", "postgres://user:password@localhost:5432/slurm_dashboard?sslmode=disable"),
		SlurmURL:        getEnv("SLURM_SERVER", "http://localhost:6820"),
		SlurmUser:       getEnv("SLURM_API_ACCOUNT", "slurm"),
		SlurmToken:      getEnv("SLURM_API_TOKEN", ""),
		SlurmAPIVersion: getEnv("SLURM_API_VERSION", "v0.0.41"),
		ClusterName:     getEnv("CLUSTER_NAME", "mycluster"),
		SyncInterval:    getEnvInt("SYNC_INTERVAL", 300),
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
