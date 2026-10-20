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
