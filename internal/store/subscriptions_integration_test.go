package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

func TestClaimSubscriptionsSkipLockedAndClock(t *testing.T) {
	tenants := newTestTenants(t)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Subs", apikey.Generate(apikey.EnvTest))
	accounts := NewAccounts(tenants.db)
	subs := NewSubscriptions(tenants.db)

	soon := seedTokenAccount(t, accounts, tenant.ID, "sub-soon-"+tenant.ID[:8], "")
	later := seedTokenAccount(t, accounts, tenant.ID, "sub-later-"+tenant.ID[:8], "")
	if err := subs.Upsert(ctx, Subscription{
		AccountID: soon, Provider: "google", ExternalID: "ext-soon-" + tenant.ID[:8],
		ResourceID: "res-soon", SecretHash: "abc", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := subs.Upsert(ctx, Subscription{
		AccountID: later, Provider: "google", ExternalID: "ext-later-" + tenant.ID[:8],
		ResourceID: "res-later", SecretHash: "abc", ExpiresAt: time.Now().Add(48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// Expiry is compared with Postgres now(), not the process clock.
	if _, err := tenants.db.Exec(ctx, `UPDATE provider_subscriptions SET expires_at = now() + interval '1 hour' WHERE account_id = $1`, soon); err != nil {
		t.Fatal(err)
	}
	if _, err := tenants.db.Exec(ctx, `UPDATE provider_subscriptions SET expires_at = now() + interval '3 days' WHERE account_id = $1`, later); err != nil {
		t.Fatal(err)
	}

	var due []string
	if err := subs.ClaimExpiring(ctx, 24*time.Hour, 1000, func(ids []string) error {
		due = ids
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	gotSoon := false
	for _, id := range due {
		if id == later {
			t.Fatal("subscription outside 24h was claimed")
		}
		if id == soon {
			gotSoon = true
		}
	}
	if !gotSoon {
		t.Fatal("subscription inside 24h was not claimed")
	}

	var held []string
	for i := 0; i < 4; i++ {
		id := seedTokenAccount(t, accounts, tenant.ID, "sub-hold-"+tenant.ID[:8]+"-"+string(rune('a'+i)), "")
		held = append(held, id)
		if err := subs.Upsert(ctx, Subscription{
			AccountID: id, Provider: "microsoft", ExternalID: "ext-hold-" + id,
			SecretHash: "abc", ExpiresAt: time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	other, err := NewPool(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	replicaB := NewSubscriptions(other)

	started := make(chan struct{})
	release := make(chan struct{})
	var idsA []string
	errA := make(chan error, 1)
	go func() {
		errA <- subs.ClaimExpiring(ctx, 48*time.Hour, 1000, func(ids []string) error {
			idsA = ids
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	var idsB []string
	if err := replicaB.ClaimExpiring(ctx, 48*time.Hour, 1000, func(ids []string) error {
		idsB = ids
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errA; err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, id := range append(append([]string{}, idsA...), idsB...) {
		seen[id]++
	}
	for _, id := range held {
		if seen[id] != 1 {
			t.Fatalf("account %s claimed %d times, want 1", id, seen[id])
		}
	}
}
