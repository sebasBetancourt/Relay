package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port          string
	DatabaseURL   string
	EncryptionKey string
	LogLevel      string
}

func Load() (Config, error) {
	cfg := Config{
		Port:          getEnv("PORT", "8080"),
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		EncryptionKey: os.Getenv("ENCRYPTION_KEY"),
		LogLevel:      getEnv("LOG_LEVEL", "info"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}