package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

func TestOAuthStatesCreate(t *testing.T) {
	tenants := newTestTenants(t)
	states := NewOAuthStates(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "OAuth", apikey.Generate(apikey.EnvTest))

	st := OAuthState{
		State:        "state-" + tenant.ID,
		TenantID:     tenant.ID,
		Provider:     "google",
		CodeVerifier: "verifier-123",
	}
	if err := states.Create(ctx, st, 10*time.Minute); err != nil {
		t.Fatalf("create: %v", err)
	}

	var (
		got     OAuthState
		ttlLeft time.Duration
		usedAt  *time.Time
	)
	// Measure the TTL with the DB clock, the same one Create used.
	err := tenants.db.QueryRow(ctx, `
		SELECT state, tenant_id::text, provider::text, code_verifier, expires_at - now(), used_at
		FROM oauth_states WHERE state = $1`, st.State,
	).Scan(&got.State, &got.TenantID, &got.Provider, &got.CodeVerifier, &ttlLeft, &usedAt)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != st {
		t.Fatalf("stored %+v, want %+v", got, st)
	}
	if ttlLeft < 9*time.Minute+50*time.Second || ttlLeft > 10*time.Minute {
		t.Fatalf("expires in %v, want ~10m", ttlLeft)
	}
	if usedAt != nil {
		t.Fatal("new state must not be marked used")
	}

	// Deleting the tenant must delete its in-flight states (ON DELETE CASCADE).
	if _, err := tenants.db.Exec(ctx, "DELETE FROM tenants WHERE id = $1", tenant.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tenants.db.QueryRow(ctx, "SELECT count(*) FROM oauth_states WHERE state = $1", st.State).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected cascade delete, %d state(s) left", n)
	}
}

func TestOAuthStatesCreateUnknownTenant(t *testing.T) {
	tenants := newTestTenants(t)
	states := NewOAuthStates(tenants.db)

	err := states.Create(context.Background(), OAuthState{
		State:        "orphan-state",
		TenantID:     "00000000-0000-0000-0000-000000000000",
		Provider:     "google",
		CodeVerifier: "v",
	}, time.Minute)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
