package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

func newTestIdempotency(t *testing.T) (*Idempotency, string) {
	t.Helper()
	tenants := newTestTenants(t)
	tenant := createTenant(t, tenants, "Idempotency", apikey.Generate(apikey.EnvTest))
	return NewIdempotency(tenants.db), tenant.ID
}

func TestIdempotencyFirstRunsThenReplays(t *testing.T) {
	s, tenant := newTestIdempotency(t)
	ctx := context.Background()

	stored, err := s.Begin(ctx, tenant, "k", "h1")
	if err != nil || stored != nil {
		t.Fatalf("first begin: %v %v", stored, err)
	}
	if _, err := s.Begin(ctx, tenant, "k", "h1"); !errors.Is(err, ErrIdempotencyInProgress) {
		t.Fatalf("while running: %v", err)
	}
	if err := s.Complete(ctx, tenant, "k", StoredResponse{Status: 201, Body: []byte(`{"id":"e1"}`)}); err != nil {
		t.Fatal(err)
	}
	stored, err = s.Begin(ctx, tenant, "k", "h1")
	if err != nil || stored == nil || stored.Status != 201 {
		t.Fatalf("replay: %+v %v", stored, err)
	}
	if _, err := s.Begin(ctx, tenant, "k", "h2"); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("other body: %v", err)
	}
}

func TestIdempotencyStoresAnEmptyResponse(t *testing.T) {
	s, tenant := newTestIdempotency(t)
	ctx := context.Background()
	if _, err := s.Begin(ctx, tenant, "k", "h"); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, tenant, "k", StoredResponse{Status: 204}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Begin(ctx, tenant, "k", "h")
	if err != nil || stored == nil || stored.Status != 204 || stored.Body != nil {
		t.Fatalf("replay: %+v %v", stored, err)
	}
}

func TestIdempotencyOnlyOneConcurrentRequestRuns(t *testing.T) {
	s, tenant := newTestIdempotency(t)
	var (
		wg      sync.WaitGroup
		claimed atomic.Int32
	)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stored, err := s.Begin(context.Background(), tenant, "k", "h")
			if err == nil && stored == nil {
				claimed.Add(1)
			} else if err != nil && !errors.Is(err, ErrIdempotencyInProgress) {
				t.Errorf("begin: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := claimed.Load(); got != 1 {
		t.Fatalf("claimed = %d, want exactly 1", got)
	}
}

func TestIdempotencyReleaseAllowsARetry(t *testing.T) {
	s, tenant := newTestIdempotency(t)
	ctx := context.Background()
	if _, err := s.Begin(ctx, tenant, "k", "h"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, tenant, "k"); err != nil {
		t.Fatal(err)
	}
	if stored, err := s.Begin(ctx, tenant, "k", "h"); err != nil || stored != nil {
		t.Fatalf("after release: %v %v", stored, err)
	}
}

func TestIdempotencyTakesOverAClaimFromADeadProcess(t *testing.T) {
	s, tenant := newTestIdempotency(t)
	ctx := context.Background()
	if _, err := s.Begin(ctx, tenant, "k", "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE idempotency_keys SET locked_at = now() - interval '2 minutes' WHERE tenant_id = $1`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(ctx, tenant, "k", "other"); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("another request must not take it: %v", err)
	}
	if stored, err := s.Begin(ctx, tenant, "k", "h"); err != nil || stored != nil {
		t.Fatalf("takeover: %v %v", stored, err)
	}
}

func TestIdempotencyKeysAreScopedToTheTenant(t *testing.T) {
	s, tenant := newTestIdempotency(t)
	ctx := context.Background()
	if _, err := s.Begin(ctx, tenant, "k", "h"); err != nil {
		t.Fatal(err)
	}
	other := createTenant(t, NewTenants(s.db), "Other", apikey.Generate(apikey.EnvTest))
	if stored, err := s.Begin(ctx, other.ID, "k", "h2"); err != nil || stored != nil {
		t.Fatalf("other tenant: %v %v", stored, err)
	}
}

func TestIdempotencyDeleteExpired(t *testing.T) {
	s, tenant := newTestIdempotency(t)
	ctx := context.Background()
	for _, key := range []string{"old", "new"} {
		if _, err := s.Begin(ctx, tenant, key, "h"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - interval '25 hours' WHERE tenant_id = $1 AND key = 'old'`, tenant); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteExpired(ctx, 1000)
	if err != nil || n < 1 {
		t.Fatalf("deleted %d: %v", n, err)
	}
	var left int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1`, tenant).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("left = %d, want the recent key", left)
	}
}
