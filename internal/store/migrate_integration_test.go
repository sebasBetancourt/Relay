package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// emptyDatabase creates a database of its own and returns its URL.
func emptyDatabase(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("relay_migrate_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		_ = conn.Close(ctx)
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func TestMigrateBuildsAnEmptyDatabaseAndIsIdempotent(t *testing.T) {
	dbURL := emptyDatabase(t)

	version, err := Migrate(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if version < 8 {
		t.Fatalf("version = %d", version)
	}
	again, err := Migrate(dbURL)
	if err != nil || again != version {
		t.Fatalf("second run: %d, %v", again, err)
	}

	ctx := context.Background()
	pool, err := NewPool(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('tenants', 'idempotency_keys')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 2 {
		t.Fatalf("tables = %d", tables)
	}
}

func TestMigrateErrorHidesThePassword(t *testing.T) {
	_, err := Migrate("postgres://relay:s3cret@127.0.0.1:1/relay?sslmode=disable")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}
