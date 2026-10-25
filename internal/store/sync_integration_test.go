package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

func TestSyncApplyIsIdempotentAndRollsBack(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	syncs := NewSyncStore(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Sync", apikey.Generate(apikey.EnvTest))
	id := seedTokenAccount(t, accounts, tenant.ID, "sync-"+tenant.ID[:8], "sync@outlook.com")

	window := time.Now().UTC().Add(30 * 24 * time.Hour)
	start := time.Now().UTC()
	first := ApplyRequest{
		AccountID: id, Expect: "", Next: "cursor-1", Full: true,
		WindowStart: start, WindowEnd: window,
		Changes: []EventChange{eventChange("e1", "Standup", "v1", false)},
	}
	if err := syncs.Apply(ctx, first); err != nil {
		t.Fatal(err)
	}
	before, err := syncs.Events(ctx, id)
	if err != nil || len(before) != 1 || before[0].Version != "v1" {
		t.Fatalf("events %+v err %v", before, err)
	}
	replay := first
	replay.Expect = "cursor-1"
	if err := syncs.Apply(ctx, replay); err != nil {
		t.Fatal(err)
	}
	again, err := syncs.Events(ctx, id)
	if err != nil || !again[0].UpdatedAt.Equal(before[0].UpdatedAt) {
		t.Fatalf("replay rewrote the row: before %s after %s", before[0].UpdatedAt, again[0].UpdatedAt)
	}

	edited := first
	edited.Expect = "cursor-1"
	edited.Next = "cursor-2"
	edited.Full = false
	edited.Changes = []EventChange{
		eventChange("e1", "Moved", "v2", false),
		eventChange("e1", "", "v3", true),
	}
	if err := syncs.Apply(ctx, edited); err != nil {
		t.Fatal(err)
	}
	rows, err := syncs.Events(ctx, id)
	if err != nil || len(rows) != 1 || !rows[0].Deleted || rows[0].Version != "v3" {
		t.Fatalf("same-page delete = %+v err %v", rows, err)
	}

	syncs.beforeCommit = func() error { return errors.New("boom") }
	failed := ApplyRequest{
		AccountID: id, Expect: "cursor-2", Next: "cursor-3",
		WindowStart: start, WindowEnd: window,
		Changes: []EventChange{eventChange("e2", "Nope", "v1", false)},
	}
	if err := syncs.Apply(ctx, failed); err == nil {
		t.Fatal("expected rollback")
	}
	st, err := syncs.State(ctx, id)
	if err != nil || st.Cursor != "cursor-2" {
		t.Fatalf("cursor = %q err %v", st.Cursor, err)
	}
	left, err := syncs.Events(ctx, id)
	if err != nil || len(left) != 1 || left[0].ProviderEventID != "e1" {
		t.Fatalf("rolled-back rows %+v", left)
	}

	// A full resync that no longer returns a live event tombstones it.
	syncs.beforeCommit = nil
	if err := syncs.Apply(ctx, ApplyRequest{
		AccountID: id, Expect: "cursor-2", Next: "cursor-3",
		WindowStart: start, WindowEnd: window,
		Changes: []EventChange{eventChange("e8", "Still", "v1", false)},
	}); err != nil {
		t.Fatal(err)
	}
	full := ApplyRequest{
		AccountID: id, Expect: "cursor-3", Next: "cursor-4", Full: true,
		WindowStart: start, WindowEnd: window,
		Changes: []EventChange{eventChange("e9", "Only", "v1", false)},
	}
	if err := syncs.Apply(ctx, full); err != nil {
		t.Fatal(err)
	}
	left, err = syncs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]StoredEvent{}
	for _, ev := range left {
		seen[ev.ProviderEventID] = ev
	}
	if !seen["e1"].Deleted || !seen["e8"].Deleted || seen["e9"].Deleted || seen["e9"].Version != "v1" {
		t.Fatalf("full resync = %+v", seen)
	}

	if _, err := accounts.Disconnect(ctx, id, tenant.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tenants.db.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM synced_events WHERE account_id = $1) +
		  (SELECT count(*) FROM account_sync_state WHERE account_id = $1)`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("disconnect left %d sync rows", n)
	}
}

func TestSyncApplyWaitsForTheAccountLock(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	syncs := NewSyncStore(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "SyncLock", apikey.Generate(apikey.EnvTest))
	id := seedTokenAccount(t, accounts, tenant.ID, "lock-"+tenant.ID[:8], "lock@outlook.com")
	window := time.Now().UTC().Add(time.Hour)
	if err := syncs.Apply(ctx, ApplyRequest{
		AccountID: id, Next: "c1", WindowStart: time.Now().UTC(), WindowEnd: window,
		Changes: []EventChange{eventChange("e1", "A", "v1", false)},
	}); err != nil {
		t.Fatal(err)
	}

	hold, err := tenants.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `SELECT cursor FROM account_sync_state WHERE account_id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- syncs.Apply(ctx, ApplyRequest{
			AccountID: id, Expect: "c1", Next: "c2",
			WindowStart: time.Now().UTC(), WindowEnd: window,
			Changes: []EventChange{eventChange("e1", "B", "v2", false)},
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("apply did not wait for the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, err := syncs.State(ctx, id)
	if err != nil || st.Cursor != "c2" {
		t.Fatalf("cursor = %q err %v", st.Cursor, err)
	}
}

func TestClaimStaleSkipsAFreshSync(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	syncs := NewSyncStore(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "SyncClaim", apikey.Generate(apikey.EnvTest))
	fresh := seedTokenAccount(t, accounts, tenant.ID, "fresh-"+tenant.ID[:8], "fresh@outlook.com")
	stale := seedTokenAccount(t, accounts, tenant.ID, "stale-"+tenant.ID[:8], "stale@outlook.com")
	if err := syncs.Apply(ctx, ApplyRequest{
		AccountID: fresh, Next: "c-fresh",
		WindowStart: time.Now().UTC(), WindowEnd: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := syncs.ClaimStale(ctx, 15*time.Minute, 1000, func(got []string) error {
		ids = append(ids, got...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen[stale] || seen[fresh] {
		t.Fatalf("stale present=%v fresh present=%v", seen[stale], seen[fresh])
	}
}

func eventChange(id, title, version string, deleted bool) EventChange {
	raw := fmt.Sprintf(`{"id":%q,"title":%q,"start":"2026-11-01T15:00:00Z","end":"2026-11-01T16:00:00Z","provider":"google"}`, id, title)
	if deleted && title == "" {
		raw = fmt.Sprintf(`{"id":%q,"provider":"google"}`, id)
	}
	return EventChange{ProviderEventID: id, Version: version, Data: json.RawMessage(raw), Deleted: deleted}
}
