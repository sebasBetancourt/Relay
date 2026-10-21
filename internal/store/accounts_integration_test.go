package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

func TestAccountsSaveConnection(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Accounts", apikey.Generate(apikey.EnvTest))

	conn := Connection{
		TenantID:          tenant.ID,
		Provider:          "google",
		ProviderAccountID: "sub-accounts-1",
		AccessEnc:         []byte("access-cipher"),
		AccessNonce:       []byte("nonce-access1"),
		RefreshEnc:        []byte("refresh-cipher"),
		RefreshNonce:      []byte("nonce-refresh1"),
		ExpiresAt:         time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
		Scopes:            []string{"openid", "https://www.googleapis.com/auth/calendar.readonly"},
	}
	id1, err := accounts.SaveConnection(ctx, conn)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if id1 == "" {
		t.Fatal("empty account id")
	}

	var accessEnc, refreshEnc []byte
	err = tenants.db.QueryRow(ctx, `
		SELECT access_token_enc, refresh_token_enc FROM oauth_tokens WHERE account_id = $1`, id1,
	).Scan(&accessEnc, &refreshEnc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(accessEnc, []byte("access-plain")) || bytes.Contains(accessEnc, []byte("access-plain")) {
		t.Fatal("plaintext must not appear in DB")
	}
	if !bytes.Equal(accessEnc, conn.AccessEnc) {
		t.Fatal("stored access ciphertext mismatch")
	}

	// Reconnect same tenant: same id, new tokens, status active.
	conn.AccessEnc = []byte("access-cipher-2")
	conn.AccessNonce = []byte("nonce-access2")
	conn.RefreshEnc = []byte("refresh-cipher-2")
	conn.RefreshNonce = []byte("nonce-refresh2")
	id2, err := accounts.SaveConnection(ctx, conn)
	if err != nil || id2 != id1 {
		t.Fatalf("reconnect id=%s err=%v want %s", id2, err, id1)
	}
	var status string
	_ = tenants.db.QueryRow(ctx, `SELECT status::text FROM connected_accounts WHERE id = $1`, id1).Scan(&status)
	if status != "active" {
		t.Fatalf("status = %q", status)
	}

	// Other tenant cannot steal the account.
	other := createTenant(t, tenants, "Other", apikey.Generate(apikey.EnvTest))
	conn.TenantID = other.ID
	_, err = accounts.SaveConnection(ctx, conn)
	if !errors.Is(err, ErrAccountTaken) {
		t.Fatalf("err = %v, want ErrAccountTaken", err)
	}

	// CASCADE: delete tenant removes account + tokens.
	if _, err := tenants.db.Exec(ctx, "DELETE FROM tenants WHERE id = $1", tenant.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = tenants.db.QueryRow(ctx, "SELECT count(*) FROM connected_accounts WHERE id = $1", id1).Scan(&n)
	if n != 0 {
		t.Fatal("expected cascade delete of connected_accounts")
	}
}

func TestAccountForTenantAndTokensLifecycle(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Lookup", apikey.Generate(apikey.EnvTest))
	other := createTenant(t, tenants, "OtherLookup", apikey.Generate(apikey.EnvTest))

	conn := Connection{
		TenantID:          tenant.ID,
		Provider:          "google",
		ProviderAccountID: "sub-lookup-1",
		AccessEnc:         []byte("access-enc"),
		AccessNonce:       []byte("access-n"),
		RefreshEnc:        []byte("refresh-enc"),
		RefreshNonce:      []byte("refresh-n"),
		ExpiresAt:         time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
		Scopes:            []string{"openid"},
	}
	id, err := accounts.SaveConnection(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}

	got, err := accounts.AccountForTenant(ctx, id, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || !got.HasTokens || !bytes.Equal(got.AccessEnc, conn.AccessEnc) {
		t.Fatalf("got %+v", got)
	}

	_, err = accounts.AccountForTenant(ctx, id, other.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("other tenant: %v", err)
	}
	_, err = accounts.AccountForTenant(ctx, "not-a-uuid", tenant.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad uuid: %v", err)
	}

	newExp := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	err = accounts.UpdateTokens(ctx, id, TokenUpdate{
		AccessEnc:   []byte("access-enc-2"),
		AccessNonce: []byte("access-n-2"),
		ExpiresAt:   newExp,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = accounts.AccountForTenant(ctx, id, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.AccessEnc, []byte("access-enc-2")) {
		t.Fatal("access not updated")
	}
	if !bytes.Equal(got.RefreshEnc, []byte("refresh-enc")) || !bytes.Equal(got.RefreshNonce, []byte("refresh-n")) {
		t.Fatal("refresh must be preserved when omitted")
	}
	if !got.ExpiresAt.Equal(newExp) {
		t.Fatalf("expires_at = %v want %v", got.ExpiresAt, newExp)
	}

	if err := accounts.MarkRevoked(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err = accounts.AccountForTenant(ctx, id, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "revoked" || got.HasTokens {
		t.Fatalf("after revoke: %+v", got)
	}

	// Reconnect reactivates.
	conn.AccessEnc = []byte("access-enc-3")
	conn.AccessNonce = []byte("access-n-3")
	conn.RefreshEnc = []byte("refresh-enc-3")
	conn.RefreshNonce = []byte("refresh-n-3")
	id2, err := accounts.SaveConnection(ctx, conn)
	if err != nil || id2 != id {
		t.Fatalf("reconnect id=%s err=%v", id2, err)
	}
	got, err = accounts.AccountForTenant(ctx, id, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || !got.HasTokens {
		t.Fatalf("reactivated: %+v", got)
	}
}

func TestAccountsPreserveRefreshWhenMissing(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "NoRefresh", apikey.Generate(apikey.EnvTest))

	conn := Connection{
		TenantID:          tenant.ID,
		Provider:          "google",
		ProviderAccountID: "sub-norefresh",
		AccessEnc:         []byte("a1"),
		AccessNonce:       []byte("n1"),
		RefreshEnc:        []byte("r1"),
		RefreshNonce:      []byte("rn1"),
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
		Scopes:            []string{"openid"},
	}
	id, err := accounts.SaveConnection(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}

	conn.AccessEnc = []byte("a2")
	conn.AccessNonce = []byte("n2")
	conn.RefreshEnc = nil
	conn.RefreshNonce = nil
	if _, err := accounts.SaveConnection(ctx, conn); err != nil {
		t.Fatal(err)
	}

	var refreshEnc, refreshNonce []byte
	err = tenants.db.QueryRow(ctx, `
		SELECT refresh_token_enc, refresh_nonce FROM oauth_tokens WHERE account_id = $1`, id,
	).Scan(&refreshEnc, &refreshNonce)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(refreshEnc, []byte("r1")) || !bytes.Equal(refreshNonce, []byte("rn1")) {
		t.Fatalf("refresh not preserved: enc=%q nonce=%q", refreshEnc, refreshNonce)
	}
}

func seedTokenAccount(t *testing.T, accounts *Accounts, tenantID, sub, email string) string {
	t.Helper()
	id, err := accounts.SaveConnection(context.Background(), Connection{
		TenantID: tenantID, Provider: "microsoft", ProviderAccountID: sub, Email: email,
		AccessEnc: []byte("access-0"), AccessNonce: []byte("nonce-a-0"),
		RefreshEnc: []byte("refresh-0"), RefreshNonce: []byte("nonce-r-0"),
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
		Scopes:    []string{"Calendars.Read"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}

func TestAccountsEmail(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Email", apikey.Generate(apikey.EnvTest))
	sub := "ms-email-" + tenant.ID[:8]

	id := seedTokenAccount(t, accounts, tenant.ID, sub, "ana@outlook.com")
	email := func() string {
		t.Helper()
		a, err := accounts.AccountForTenant(ctx, id, tenant.ID)
		if err != nil {
			t.Fatal(err)
		}
		return a.Email
	}
	if got := email(); got != "ana@outlook.com" {
		t.Fatalf("email = %q", got)
	}

	// Reconnect without an email (provider returned none): keep the stored one.
	seedTokenAccount(t, accounts, tenant.ID, sub, "")
	if got := email(); got != "ana@outlook.com" {
		t.Fatalf("empty email on reconnect overwrote it: %q", got)
	}

	// The user renamed their mailbox: the new email wins.
	seedTokenAccount(t, accounts, tenant.ID, sub, "ana.p@outlook.com")
	if got := email(); got != "ana.p@outlook.com" {
		t.Fatalf("email = %q, want the new one", got)
	}
}

func TestRefreshLockedAppliesUpdate(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "RefreshLocked", apikey.Generate(apikey.EnvTest))
	id := seedTokenAccount(t, accounts, tenant.ID, "ms-lock-"+tenant.ID[:8], "")

	newExp := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	err := accounts.RefreshLocked(ctx, id, func(cur StoredAccount) (*TokenUpdate, error) {
		if cur.ID != id || !bytes.Equal(cur.RefreshEnc, []byte("refresh-0")) || cur.TenantID != tenant.ID {
			t.Errorf("fn got %+v", cur)
		}
		return &TokenUpdate{
			AccessEnc: []byte("access-1"), AccessNonce: []byte("nonce-a-1"),
			RefreshEnc: []byte("refresh-1"), RefreshNonce: []byte("nonce-r-1"),
			ExpiresAt: newExp,
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := accounts.AccountForTenant(ctx, id, tenant.ID)
	if !bytes.Equal(a.RefreshEnc, []byte("refresh-1")) || !bytes.Equal(a.RefreshNonce, []byte("nonce-r-1")) || !a.ExpiresAt.Equal(newExp) {
		t.Fatalf("update not stored: %+v", a)
	}

	// fn error → rollback: nothing changes.
	boom := errors.New("provider down")
	err = accounts.RefreshLocked(ctx, id, func(StoredAccount) (*TokenUpdate, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want fn error", err)
	}
	b, _ := accounts.AccountForTenant(ctx, id, tenant.ID)
	if !bytes.Equal(b.AccessEnc, a.AccessEnc) {
		t.Fatal("a failed fn must not change the row")
	}

	// Revoked accounts have no token row → ErrNotFound, fn never runs.
	if err := accounts.MarkRevoked(ctx, id); err != nil {
		t.Fatal(err)
	}
	err = accounts.RefreshLocked(ctx, id, func(StoredAccount) (*TokenUpdate, error) {
		t.Error("fn must not run for a revoked account")
		return nil, nil
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestRefreshLockedSerializesReplicas uses two pools, as two API replicas
// would: the second caller waits for the first and then sees its update.
func TestRefreshLockedSerializesReplicas(t *testing.T) {
	tenants := newTestTenants(t)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Replicas", apikey.Generate(apikey.EnvTest))
	replicaA := NewAccounts(tenants.db)
	id := seedTokenAccount(t, replicaA, tenant.ID, "ms-replicas-"+tenant.ID[:8], "")

	otherPool, err := NewPool(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(otherPool.Close)
	replicaB := NewAccounts(otherPool)

	inA := make(chan struct{})
	var bSaw []byte
	errs := make(chan error, 2)

	go func() {
		errs <- replicaA.RefreshLocked(ctx, id, func(StoredAccount) (*TokenUpdate, error) {
			close(inA)
			time.Sleep(200 * time.Millisecond) // a slow provider call while holding the lock
			return &TokenUpdate{
				AccessEnc: []byte("access-A"), AccessNonce: []byte("nonce-a-A"),
				RefreshEnc: []byte("refresh-A"), RefreshNonce: []byte("nonce-r-A"),
				ExpiresAt: time.Now().UTC().Add(time.Hour),
			}, nil
		})
	}()
	<-inA // A holds the row lock now
	start := time.Now()
	go func() {
		errs <- replicaB.RefreshLocked(ctx, id, func(cur StoredAccount) (*TokenUpdate, error) {
			bSaw = cur.RefreshEnc
			return nil, nil
		})
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("replica B did not wait for the lock (%v)", waited)
	}
	if !bytes.Equal(bSaw, []byte("refresh-A")) {
		t.Fatalf("replica B saw %q, want A's rotated refresh token", bSaw)
	}
}

func TestListForTenantKeysetPaging(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "List", apikey.Generate(apikey.EnvTest))
	other := createTenant(t, tenants, "List other", apikey.Generate(apikey.EnvTest))

	var want []string
	for i := range 5 {
		want = append(want, seedTokenAccount(t, accounts, tenant.ID, fmt.Sprintf("list-%s-%d", tenant.ID[:8], i), fmt.Sprintf("u%d@ex.com", i)))
	}
	seedTokenAccount(t, accounts, other.ID, "list-other-"+other.ID[:8], "x@ex.com")

	var got []string
	var after *AccountCursor
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging did not terminate")
		}
		rows, err := accounts.ListForTenant(ctx, tenant.ID, 2, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Provider != "microsoft" || r.Status != "active" || r.Email == "" || r.ConnectedAt.IsZero() {
				t.Fatalf("row = %+v", r)
			}
			got = append(got, r.ID)
		}
		if len(rows) < 2 {
			break
		}
		last := rows[len(rows)-1]
		after = &AccountCursor{CreatedAt: last.ConnectedAt, ID: last.ID}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("paged ids = %v, want %v (creation order, no other tenant)", got, want)
	}
}

func TestDisconnect(t *testing.T) {
	tenants := newTestTenants(t)
	accounts := NewAccounts(tenants.db)
	ctx := context.Background()
	tenant := createTenant(t, tenants, "Disconnect", apikey.Generate(apikey.EnvTest))
	other := createTenant(t, tenants, "Disconnect other", apikey.Generate(apikey.EnvTest))
	sub := "disc-" + tenant.ID[:8]
	id := seedTokenAccount(t, accounts, tenant.ID, sub, "ana@ex.com")

	// Another tenant cannot disconnect it, and learns nothing.
	if _, err := accounts.Disconnect(ctx, id, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other tenant: err = %v, want ErrNotFound", err)
	}
	if _, err := accounts.Disconnect(ctx, "not-a-uuid", tenant.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id: err = %v, want ErrNotFound", err)
	}

	prev, err := accounts.Disconnect(ctx, id, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prev.Status != "active" || !prev.HasTokens || !bytes.Equal(prev.RefreshEnc, []byte("refresh-0")) {
		t.Fatalf("prev = %+v, want the account as it was, with tokens", prev)
	}
	now, _ := accounts.AccountForTenant(ctx, id, tenant.ID)
	if now.Status != "disconnected" || now.HasTokens {
		t.Fatalf("after = %+v, want disconnected without tokens", now)
	}

	// Idempotent: the second call succeeds and says it was already done.
	again, err := accounts.Disconnect(ctx, id, tenant.ID)
	if err != nil || again.Status != "disconnected" || again.HasTokens {
		t.Fatalf("second disconnect = %+v, %v", again, err)
	}

	// Connecting the same provider account again reactivates the same row.
	if id2 := seedTokenAccount(t, accounts, tenant.ID, sub, ""); id2 != id {
		t.Fatalf("reconnect id = %s, want %s", id2, id)
	}
	back, _ := accounts.AccountForTenant(ctx, id, tenant.ID)
	if back.Status != "active" || !back.HasTokens || back.Email != "ana@ex.com" {
		t.Fatalf("reconnected = %+v", back)
	}
}
