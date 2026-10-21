package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

type accountsList struct {
	Accounts []struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		Email    string `json:"email"`
		Status   string `json:"status"`
	} `json:"accounts"`
	NextCursor string `json:"next_cursor"`
}

func (f *flow) listAccounts() accountsList {
	f.t.Helper()
	rec := f.do(http.MethodGet, "/v1/accounts", f.authHdr())
	if rec.Code != http.StatusOK {
		f.t.Fatalf("list accounts %d %s", rec.Code, rec.Body.String())
	}
	var out accountsList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// One tenant with a Google and an Outlook account: list, disconnect,
// reconnect, all through the public API.
func TestAccountsLifecycle(t *testing.T) {
	f := newFlow(t)
	googleID := f.connectThrough("google")
	msID := f.connectThrough("microsoft")

	list := f.listAccounts()
	if len(list.Accounts) != 2 || list.NextCursor != "" {
		t.Fatalf("accounts = %+v, want 2 on one page", list)
	}
	got := map[string]string{}
	for _, a := range list.Accounts {
		if a.Status != "active" {
			t.Fatalf("account %+v not active", a)
		}
		got[a.Provider] = a.Email
	}
	if got["google"] != "flow@gmail.com" || got["microsoft"] != "flow@outlook.com" {
		t.Fatalf("emails = %v", got)
	}

	// Another tenant can neither disconnect nor see them.
	otherKey := apikey.Generate(apikey.EnvTest)
	other, err := f.tenants.Create(f.ctx, "Accounts other", otherKey.Hash, otherKey.KeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", other.ID) })
	otherHdr := map[string]string{"Authorization": "Bearer " + otherKey.Plaintext}
	if rec := f.do(http.MethodDelete, "/v1/accounts/"+msID, otherHdr); !hasError(rec, http.StatusNotFound, "not_found") {
		t.Fatalf("other tenant delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodGet, "/v1/accounts", otherHdr); rec.Body.String() != "{\"accounts\":[]}\n" {
		t.Fatalf("other tenant list: %s", rec.Body.String())
	}

	// Disconnect Outlook: 204, idempotent, events say so, tokens are gone.
	for range 2 {
		if rec := f.do(http.MethodDelete, "/v1/accounts/"+msID, f.authHdr()); rec.Code != http.StatusNoContent {
			t.Fatalf("delete microsoft: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := f.events(msID, ""); !hasError(rec, http.StatusConflict, "account_disconnected") {
		t.Fatalf("events after disconnect: %d %s", rec.Code, rec.Body.String())
	}
	if row := f.row(msID); row.Status != "disconnected" || row.HasTokens {
		t.Fatalf("microsoft row = %+v", row)
	}

	// Disconnect Google: its refresh token is revoked at (fake) Google.
	refresh := f.storedRefresh("google", googleID)
	if rec := f.do(http.MethodDelete, "/v1/accounts/"+googleID, f.authHdr()); rec.Code != http.StatusNoContent {
		t.Fatalf("delete google: %d %s", rec.Code, rec.Body.String())
	}
	if f.google.Revokes() != 1 || f.google.RefreshTokenValid(refresh) {
		t.Fatalf("google revocations = %d, refresh still valid = %v", f.google.Revokes(), f.google.RefreshTokenValid(refresh))
	}
	for _, a := range f.listAccounts().Accounts {
		if a.Status != "disconnected" {
			t.Fatalf("account %+v, want disconnected", a)
		}
	}

	// Reconnecting Outlook brings the same account back.
	if id := f.connectThrough("microsoft"); id != msID {
		t.Fatalf("reconnect account_id = %s, want %s", id, msID)
	}
	if rec := f.events(msID, ""); rec.Code != http.StatusOK {
		t.Fatalf("events after reconnect: %d %s", rec.Code, rec.Body.String())
	}
}
