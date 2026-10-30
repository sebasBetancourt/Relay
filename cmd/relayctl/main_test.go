package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/queue"
	"github.com/sebasBetancourt/relay/internal/queue/natsq"
	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/testutil/natsjs"
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
		{"dlq without action", []string{"dlq"}, okEnv, "Usage:"},
		{"dlq unknown action", []string{"dlq", "drop"}, okEnv, "Usage:"},
		{"dlq replay without seq", []string{"dlq", "replay"}, okEnv, "Usage:"},
		{"dlq replay not a number", []string{"dlq", "replay", "abc"}, okEnv, "positive integer"},
		{"dlq replay zero", []string{"dlq", "replay", "0"}, okEnv, "positive integer"},
		{"dlq list missing nats", []string{"dlq", "list"}, map[string]string{}, "NATS_URL is required"},
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

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"version"}, &stdout, &stderr, envFrom(nil)); code != exitOK {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.HasPrefix(stdout.String(), "relayctl ") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestDLQListAndReplay(t *testing.T) {
	url := natsjs.Start(t)
	c, err := natsq.Connect(context.Background(), url, natsq.WithMaxDeliver(2),
		natsq.WithAckWait(20*time.Millisecond), natsq.WithBackOff(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan struct{})
	go func() {
		_ = c.Consume(ctx, "relay.sync.account", func(context.Context, queue.Message) error {
			return queue.Permanent(errors.New("account not found"))
		})
		close(got)
	}()
	body := []byte(`{"account_id":"acct-1"}`)
	if err := c.Publish(context.Background(), "relay.sync.account", "job-1", body); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		letters, err := c.ListDLQ(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(letters) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("message was not dead-lettered")
		}
		time.Sleep(15 * time.Millisecond)
	}
	cancel()
	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("consumer did not stop")
	}

	env := envFrom(map[string]string{"NATS_URL": url})
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"dlq", "list"}, &stdout, &stderr, env); code != exitOK {
		t.Fatalf("list exit %d stderr %s", code, stderr.String())
	}
	fields := strings.Split(strings.TrimSpace(stdout.String()), "\t")
	if len(fields) < 4 || fields[1] != "relay.sync.account" || !strings.Contains(fields[3], "account not found") {
		t.Fatalf("list stdout %q", stdout.String())
	}
	seq := fields[0]

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"dlq", "replay", seq}, &stdout, &stderr, env); code != exitOK {
		t.Fatalf("replay exit %d stderr %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "replayed sequence "+seq) {
		t.Fatalf("stderr %q", stderr.String())
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	replayed := make(chan []byte, 1)
	go func() {
		_ = c.Consume(ctx2, "relay.sync.account", func(_ context.Context, msg queue.Message) error {
			replayed <- append([]byte(nil), msg.Payload...)
			return nil
		})
	}()
	select {
	case payload := <-replayed:
		if string(payload) != string(body) {
			t.Fatalf("replayed %s", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replayed job was not delivered")
	}
}
