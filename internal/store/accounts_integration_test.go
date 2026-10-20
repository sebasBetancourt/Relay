package store

import (
	"bytes"
	"context"
	"errors"
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
