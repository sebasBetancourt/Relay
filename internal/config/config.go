package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

// encryptionKeySize is the key length AES-256-GCM requires.
const encryptionKeySize = 32

type Config struct {
	Port        string
	DatabaseURL string
	LogLevel    string

	// Env is RELAY_ENV: which API key prefix (rly_live_ / rly_test_) this
	// process accepts. Required, with no default, so production can never
	// start in test mode by accident (or the other way around).
	Env apikey.Env

	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string

	// EncryptionKey is the decoded ENCRYPTION_KEY (32 bytes). Never log it.
	EncryptionKey []byte
}

// Load reads the environment and fails fast: every missing or invalid
// variable is reported together so the process never starts half-configured.
func Load() (Config, error) {
	cfg := Config{
		Port:               getEnv("PORT", "8080"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		LogLevel:           getEnv("LOG_LEVEL", "info"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURL:  os.Getenv("GOOGLE_REDIRECT_URL"),
	}

	var errs []error
	// Ordered slice (not a map) so error messages are deterministic.
	required := []struct{ name, value string }{
		{"DATABASE_URL", cfg.DatabaseURL},
		{"GOOGLE_CLIENT_ID", cfg.GoogleClientID},
		{"GOOGLE_CLIENT_SECRET", cfg.GoogleClientSecret},
	}
	for _, r := range required {
		if r.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", r.name))
		}
	}

	env, err := parseEnv(os.Getenv("RELAY_ENV"))
	if err != nil {
		errs = append(errs, fmt.Errorf("RELAY_ENV: %w", err))
	}
	cfg.Env = env

	if err := validateRedirectURL(cfg.GoogleRedirectURL); err != nil {
		errs = append(errs, fmt.Errorf("GOOGLE_REDIRECT_URL: %w", err))
	}

	key, err := decodeEncryptionKey(os.Getenv("ENCRYPTION_KEY"))
	if err != nil {
		errs = append(errs, fmt.Errorf("ENCRYPTION_KEY: %w", err))
	}
	cfg.EncryptionKey = key

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// parseEnv is strict on purpose: "prod" or "LIVE" are typos we want to see
// at startup, not silently mapped to some mode.
func parseEnv(raw string) (apikey.Env, error) {
	switch env := apikey.Env(raw); env {
	case apikey.EnvLive, apikey.EnvTest:
		return env, nil
	case "":
		return "", errors.New(`is required ("live" or "test")`)
	default:
		return "", fmt.Errorf(`must be "live" or "test", got %q`, raw)
	}
}

func validateRedirectURL(raw string) error {
	if raw == "" {
		return errors.New("is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an absolute http(s) URL")
	}
	return nil
}

// decodeEncryptionKey never includes the value in its errors.
func decodeEncryptionKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("is required (generate one with: openssl rand -base64 32)")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("must be valid base64")
	}
	if len(key) != encryptionKeySize {
		return nil, fmt.Errorf("must decode to %d bytes, got %d", encryptionKeySize, len(key))
	}
	return key, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
