package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/store"
)

// TestV1MeWithRealDB runs the whole chain: router → RequireAPIKey → store →
// Postgres, then revokes the key and expects the door to close.
func TestV1MeWithRealDB(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()

	pool, err := store.NewPool(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	tenants := store.NewTenants(pool)
	key := apikey.Generate(apikey.EnvTest)
	tenant, err := tenants.Create(ctx, "Integration", key.Hash, key.KeyPrefix)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID)
	})

	router := NewRouter(discardLogger(), pool, tenants, apikey.EnvTest)
	call := func() int {
		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+key.Plaintext)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call(); code != http.StatusOK {
		t.Fatalf("before revoke: status = %d, want 200", code)
	}
	if err := tenants.RevokeKey(ctx, key.Hash); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code := call(); code != http.StatusUnauthorized {
		t.Fatalf("after revoke: status = %d, want 401", code)
	}
}
