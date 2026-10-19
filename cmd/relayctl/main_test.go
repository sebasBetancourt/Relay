package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/store"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestRunUsageErrors(t *testing.T) {
	okEnv := map[string]string{"DATABASE_URL": "postgres://unused", "RELAY_ENV": "test"}

	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		wantErr string
	}{
		{"no args", nil, okEnv, "Usage:"},
		{"unknown command", []string{"tenant", "delete"}, okEnv, "Usage:"},
		{"missing name", []string{"tenant", "create"}, okEnv, "Usage:"},
		{"blank name", []string{"tenant", "create", "--name", "   "}, okEnv, "Usage:"},
		{"extra args", []string{"tenant", "create", "--name", "Acme", "oops"}, okEnv, "Usage:"},
		{"unknown flag", []string{"tenant", "create", "--nam", "Acme"}, okEnv, "flag provided but not defined"},
		{"missing env vars", []string{"tenant", "create", "--name", "Acme"}, map[string]string{}, "DATABASE_URL is required"},
		{"bad relay env", []string{"tenant", "create", "--name", "Acme"},
			map[string]string{"DATABASE_URL": "postgres://unused", "RELAY_ENV": "prod"}, `got "prod"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr, envFrom(tt.env))

			if code != exitUsage {
				t.Fatalf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout must stay empty on error, got %q", stdout.String())
			}
		})
	}
}

func TestTenantCreateWithRealDB(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	env := envFrom(map[string]string{"DATABASE_URL": url, "RELAY_ENV": "test"})

	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"tenant", "create", "--name", "CLI Test"}, &stdout, &stderr, env)
	if code != exitOK {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr.String())
	}

	// stdout is exactly the key and nothing else, so `> key.txt` is safe.
	key := strings.TrimSpace(stdout.String())
	if !apikey.ValidForEnv(key, apikey.EnvTest) {
		t.Fatalf("stdout is not a valid test key: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), key) {
		t.Fatal("the key must not be printed to stderr")
	}

	pool, err := store.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	tenant, err := store.NewTenants(pool).ByAPIKeyHash(ctx, apikey.Hash(key))
	if err != nil {
		t.Fatalf("created key not found: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID)
	})
	if tenant.Name != "CLI Test" {
		t.Fatalf("tenant name = %q", tenant.Name)
	}
}
