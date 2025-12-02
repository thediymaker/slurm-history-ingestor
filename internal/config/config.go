package config

import (
	"os"
	"strconv"
)

type Config struct {
	DatabaseURL     string
	SlurmURL        string
	SlurmUser       string
	SlurmToken      string
	SlurmAPIVersion string
	ClusterName     string
	SyncInterval    int // Seconds
}

func Load() *Config {
	return &Config{
		DatabaseURL:     getEnv("DATABASE_URL", "postgres://user:password@localhost:5432/slurm_dashboard?sslmode=disable"),
		SlurmURL:        getEnv("SLURM_SERVER", "http://localhost:6820"),
		SlurmUser:       getEnv("SLURM_API_ACCOUNT", "slurm"),
		SlurmToken:      getEnv("SLURM_API_TOKEN", ""),
		SlurmAPIVersion: getEnv("SLURM_API_VERSION", "v0.0.41"),
		ClusterName:     getEnv("CLUSTER_NAME", "mycluster"),
		SyncInterval:    getEnvInt("SYNC_INTERVAL", 300),
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
