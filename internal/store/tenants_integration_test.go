package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

func newTestTenants(t *testing.T) *Tenants {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping integration test")
	}
	pool, err := NewPool(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewTenants(pool)
}

// createTenant inserts a tenant and deletes it when the test ends.
func createTenant(t *testing.T, s *Tenants, name string, key apikey.Key) Tenant {
	t.Helper()
	tenant, err := s.Create(context.Background(), name, key.Hash, key.KeyPrefix)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.db.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID)
	})
	return tenant
}

func TestTenantsCreateAndLookup(t *testing.T) {
	s := newTestTenants(t)
	ctx := context.Background()
	key := apikey.Generate(apikey.EnvTest)

	created := createTenant(t, s, "  Acme  ", key)
	if created.ID == "" || created.CreatedAt.IsZero() {
		t.Fatalf("expected id and created_at, got %+v", created)
	}
	if created.Name != "Acme" {
		t.Fatalf("expected trimmed name Acme, got %q", created.Name)
	}

	got, err := s.ByAPIKeyHash(ctx, key.Hash)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("lookup returned %+v, want id %s", got, created.ID)
	}
}

func TestTenantsLookupUsesCache(t *testing.T) {
	s := newTestTenants(t)
	ctx := context.Background()
	key := apikey.Generate(apikey.EnvTest)
	createTenant(t, s, "Cached", key)

	if _, err := s.ByAPIKeyHash(ctx, key.Hash); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	// Second hit must succeed from cache even if we cannot prove skip-DB here;
	// we assert the entry is present and not expired.
	if _, ok := s.cacheGet(key.Hash); !ok {
		t.Fatal("expected hash in cache after lookup")
	}
}

func TestTenantsLookupUnknownHash(t *testing.T) {
	s := newTestTenants(t)

	_, err := s.ByAPIKeyHash(context.Background(), apikey.Hash("rly_does_not_exist"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestTenantsDuplicateHashRejected(t *testing.T) {
	s := newTestTenants(t)
	key := apikey.Generate(apikey.EnvTest)
	createTenant(t, s, "First", key)

	if _, err := s.Create(context.Background(), "Second", key.Hash, key.KeyPrefix); err == nil {
		t.Fatal("expected unique violation on duplicate key_hash")
	}
}

func TestTenantsAddKeyAndRevoke(t *testing.T) {
	s := newTestTenants(t)
	ctx := context.Background()
	first := apikey.Generate(apikey.EnvTest)
	second := apikey.Generate(apikey.EnvTest)
	tenant := createTenant(t, s, "Multi", first)

	if err := s.AddKey(ctx, tenant.ID, second.Hash, second.KeyPrefix); err != nil {
		t.Fatalf("add key: %v", err)
	}
	got, err := s.ByAPIKeyHash(ctx, second.Hash)
	if err != nil || got.ID != tenant.ID {
		t.Fatalf("second key lookup: got %+v err %v", got, err)
	}

	if err := s.RevokeKey(ctx, second.Hash); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err = s.ByAPIKeyHash(ctx, second.Hash)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected ErrRevoked, got %v", err)
	}
	// First key still works.
	if _, err := s.ByAPIKeyHash(ctx, first.Hash); err != nil {
		t.Fatalf("first key after revoke: %v", err)
	}
}

func TestTenantsCreateRejectsBadName(t *testing.T) {
	s := newTestTenants(t)
	key := apikey.Generate(apikey.EnvTest)

	for _, name := range []string{"", "   ", strings.Repeat("a", maxTenantNameLen+1)} {
		if _, err := s.Create(context.Background(), name, key.Hash, key.KeyPrefix); err == nil {
			t.Fatalf("expected error for name %q (runes=%d)", name, utf8.RuneCountInString(strings.TrimSpace(name)))
		}
	}
}

func TestTenantsNameCheckConstraint(t *testing.T) {
	s := newTestTenants(t)
	ctx := context.Background()
	// Bypass Go validation: insert directly and expect the DB CHECK to reject.
	_, err := s.db.Exec(ctx, `INSERT INTO tenants (name) VALUES ($1)`, strings.Repeat("x", maxTenantNameLen+1))
	if err == nil {
		t.Fatal("expected CHECK constraint to reject long name")
	}
}

func TestTenantsRevokeInvalidatesCache(t *testing.T) {
	s := newTestTenants(t)
	ctx := context.Background()
	key := apikey.Generate(apikey.EnvTest)
	createTenant(t, s, "RevCache", key)

	if _, err := s.ByAPIKeyHash(ctx, key.Hash); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, key.Hash); err != nil {
		t.Fatal(err)
	}
	// Even within TTL, revoked key must not come from cache as active.
	_, err := s.ByAPIKeyHash(ctx, key.Hash)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected ErrRevoked after revoke, got %v", err)
	}
}

func TestTenantsCreateRollsBackOnDuplicateKey(t *testing.T) {
	s := newTestTenants(t)
	ctx := context.Background()
	key := apikey.Generate(apikey.EnvTest)
	createTenant(t, s, "Original", key)

	name := "Orphan-" + key.KeyPrefix
	if _, err := s.Create(ctx, name, key.Hash, key.KeyPrefix); err == nil {
		t.Fatal("expected duplicate key error")
	}
	// The transaction must not leave a tenant without a key behind.
	var n int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM tenants WHERE name = $1", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected rollback, found %d orphan tenant(s)", n)
	}
}

func TestTenantsAddKeyUnknownTenant(t *testing.T) {
	s := newTestTenants(t)
	key := apikey.Generate(apikey.EnvTest)

	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if err := s.AddKey(context.Background(), id, key.Hash, key.KeyPrefix); !errors.Is(err, ErrNotFound) {
			t.Fatalf("tenant %q: expected ErrNotFound, got %v", id, err)
		}
	}
}

func TestTenantsRevokeUnknownKey(t *testing.T) {
	s := newTestTenants(t)

	err := s.RevokeKey(context.Background(), apikey.Hash("rly_does_not_exist"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
