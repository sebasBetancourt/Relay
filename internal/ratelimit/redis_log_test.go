package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestRedisLogWritesJSONAndHidesPassword(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	redisLog{log: logger}.Printf(context.Background(), "dial %s failed: %v",
		"redis://user:s3cret@host:6379/0", "connection refused")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q", buf.String())
	}
	msg, _ := entry["msg"].(string)
	if strings.Contains(msg, "s3cret") {
		t.Fatalf("message leaks the password: %q", msg)
	}
	if !strings.Contains(msg, "redis://user:***@host:6379/0") {
		t.Fatalf("message does not keep the redacted URL: %q", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Fatalf("message drops the original error: %q", msg)
	}
	if entry["level"] != "WARN" {
		t.Fatalf("level = %v, want WARN", entry["level"])
	}
}
