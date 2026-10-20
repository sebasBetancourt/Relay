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

func TestOAuthStatesConsume(t *testing.T) {
	tenants := newTestTenants(t)
	states := NewOAuthStates(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Consume", apikey.Generate(apikey.EnvTest))

	st := OAuthState{
		State:        "consume-" + tenant.ID,
		TenantID:     tenant.ID,
		Provider:     "google",
		CodeVerifier: "verifier-xyz",
	}
	if err := states.Create(ctx, st, 10*time.Minute); err != nil {
		t.Fatal(err)
	}

	got, err := states.Consume(ctx, st.State, "google")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if got.TenantID != tenant.ID || got.CodeVerifier != st.CodeVerifier {
		t.Fatalf("got %+v", got)
	}

	var n int
	if err := tenants.db.QueryRow(ctx, "SELECT count(*) FROM oauth_states WHERE state = $1", st.State).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("row must be deleted after Consume")
	}

	_, err = states.Consume(ctx, st.State, "google")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("second consume: %v", err)
	}
}

func TestOAuthStatesConsumeExpired(t *testing.T) {
	tenants := newTestTenants(t)
	states := NewOAuthStates(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Expired", apikey.Generate(apikey.EnvTest))

	st := OAuthState{
		State:        "expired-" + tenant.ID,
		TenantID:     tenant.ID,
		Provider:     "google",
		CodeVerifier: "v",
	}
	if err := states.Create(ctx, st, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	_, err := states.Consume(ctx, st.State, "google")
	if !errors.Is(err, ErrStateExpired) {
		t.Fatalf("err = %v, want ErrStateExpired", err)
	}
	var n int
	_ = tenants.db.QueryRow(ctx, "SELECT count(*) FROM oauth_states WHERE state = $1", st.State).Scan(&n)
	if n != 0 {
		t.Fatal("expired row must still be deleted")
	}
}

func TestOAuthStatesDeleteExpired(t *testing.T) {
	tenants := newTestTenants(t)
	states := NewOAuthStates(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Purge", apikey.Generate(apikey.EnvTest))

	prefix := "purge-" + tenant.ID[:8] + "-"
	for i := 0; i < 5; i++ {
		st := OAuthState{
			State: prefix + "old-" + string(rune('a'+i)), TenantID: tenant.ID,
			Provider: "google", CodeVerifier: "v",
		}
		if err := states.Create(ctx, st, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		st := OAuthState{
			State: prefix + "new-" + string(rune('a'+i)), TenantID: tenant.ID,
			Provider: "google", CodeVerifier: "v",
		}
		if err := states.Create(ctx, st, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	_, err := tenants.db.Exec(ctx, `
		UPDATE oauth_states SET expires_at = now() - interval '1 minute'
		WHERE state LIKE $1`, prefix+"old-%")
	if err != nil {
		t.Fatal(err)
	}

	// DeleteExpired removes any expired row in the DB (other packages may
	// leave some), so assert on our own keys — not exact batch totals.
	for i := 0; i < 10; i++ {
		if _, err := states.DeleteExpired(ctx, 2); err != nil {
			t.Fatal(err)
		}
		var dead int
		_ = tenants.db.QueryRow(ctx, `SELECT count(*) FROM oauth_states WHERE state LIKE $1`, prefix+"old-%").Scan(&dead)
		if dead == 0 {
			break
		}
	}
	var dead int
	_ = tenants.db.QueryRow(ctx, `SELECT count(*) FROM oauth_states WHERE state LIKE $1`, prefix+"old-%").Scan(&dead)
	if dead != 0 {
		t.Fatalf("dead left = %d", dead)
	}
	var alive int
	err = tenants.db.QueryRow(ctx, `SELECT count(*) FROM oauth_states WHERE state LIKE $1`, prefix+"new-%").Scan(&alive)
	if err != nil || alive != 2 {
		t.Fatalf("alive = %d err=%v", alive, err)
	}

	// Parallel DeleteExpired must not error (SKIP LOCKED).
	for i := 0; i < 10; i++ {
		st := OAuthState{
			State: prefix + "par-" + string(rune('a'+i)), TenantID: tenant.ID,
			Provider: "google", CodeVerifier: "v",
		}
		if err := states.Create(ctx, st, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = tenants.db.Exec(ctx, `
		UPDATE oauth_states SET expires_at = now() - interval '1 minute'
		WHERE state LIKE $1`, prefix+"par-%")

	errCh := make(chan error, 2)
	var n1, n2 int64
	go func() {
		var e error
		n1, e = states.DeleteExpired(ctx, 100)
		errCh <- e
	}()
	go func() {
		var e error
		n2, e = states.DeleteExpired(ctx, 100)
		errCh <- e
	}()
	for range 2 {
		if e := <-errCh; e != nil {
			t.Fatal(e)
		}
	}
	if n1+n2 < 10 {
		// May delete exactly 10; both succeed.
		t.Fatalf("parallel deleted %d+%d", n1, n2)
	}
	var left int
	_ = tenants.db.QueryRow(ctx, `SELECT count(*) FROM oauth_states WHERE state LIKE $1`, prefix+"par-%").Scan(&left)
	if left != 0 {
		t.Fatalf("par left = %d", left)
	}
}

func TestOAuthStatesConsumeWrongProvider(t *testing.T) {
	tenants := newTestTenants(t)
	states := NewOAuthStates(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "WrongProv", apikey.Generate(apikey.EnvTest))

	st := OAuthState{
		State:        "wrongprov-" + tenant.ID,
		TenantID:     tenant.ID,
		Provider:     "google",
		CodeVerifier: "v",
	}
	if err := states.Create(ctx, st, time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err := states.Consume(ctx, st.State, "microsoft")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
