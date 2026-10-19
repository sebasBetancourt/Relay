package config

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

var validKey = base64.StdEncoding.EncodeToString(make([]byte, 32))

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://relay:relay@localhost:5432/relay")
	t.Setenv("GOOGLE_CLIENT_ID", "client-id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "client-secret")
	t.Setenv("GOOGLE_REDIRECT_URL", "http://localhost:8080/v1/oauth/google/callback")
	t.Setenv("ENCRYPTION_KEY", validKey)
	t.Setenv("RELAY_ENV", "test")
}

func TestLoadValid(t *testing.T) {
	setValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.EncryptionKey) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(cfg.EncryptionKey))
	}
	if cfg.Env != apikey.EnvTest {
		t.Fatalf("expected env test, got %q", cfg.Env)
	}
	if cfg.Port != "8080" {
		t.Fatalf("expected default port 8080, got %q", cfg.Port)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{"missing database url", "DATABASE_URL", "", "DATABASE_URL is required"},
		{"missing client id", "GOOGLE_CLIENT_ID", "", "GOOGLE_CLIENT_ID is required"},
		{"missing client secret", "GOOGLE_CLIENT_SECRET", "", "GOOGLE_CLIENT_SECRET is required"},
		{"missing redirect url", "GOOGLE_REDIRECT_URL", "", "GOOGLE_REDIRECT_URL: is required"},
		{"relative redirect url", "GOOGLE_REDIRECT_URL", "/v1/oauth/google/callback", "absolute http(s) URL"},
		{"wrong scheme redirect url", "GOOGLE_REDIRECT_URL", "ftp://localhost/cb", "absolute http(s) URL"},
		{"missing encryption key", "ENCRYPTION_KEY", "", "ENCRYPTION_KEY: is required"},
		{"encryption key not base64", "ENCRYPTION_KEY", "not base64!!", "must be valid base64"},
		{"missing relay env", "RELAY_ENV", "", "RELAY_ENV: is required"},
		{"unknown relay env", "RELAY_ENV", "prod", `must be "live" or "test", got "prod"`},
		{"uppercase relay env", "RELAY_ENV", "LIVE", `got "LIVE"`},
		{"encryption key too short", "ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)), "got 16"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := Load()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	setValidEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ENCRYPTION_KEY", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, want := range []string{"DATABASE_URL", "ENCRYPTION_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to mention %s, got %q", want, err)
		}
	}
}

func TestLoadNeverLeaksEncryptionKey(t *testing.T) {
	setValidEnv(t)
	secret := base64.StdEncoding.EncodeToString([]byte("sixteen-byte-key"))
	t.Setenv("ENCRYPTION_KEY", secret)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error message leaks the encryption key")
	}
}

func TestLoadLiveEnv(t *testing.T) {
	setValidEnv(t)
	t.Setenv("RELAY_ENV", "live")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Env != apikey.EnvLive {
		t.Fatalf("expected env live, got %q", cfg.Env)
	}
}
