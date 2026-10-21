package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/vault"
)

// The same contract, checked for every provider.

func TestFlowHappyPath(t *testing.T) {
	for _, p := range providers {
		t.Run(p, func(t *testing.T) {
			f := newFlow(t)
			fake := f.fake(p)

			authURL := f.connect(p)
			state := mustURLQuery(t, authURL, "state")
			rec := f.callback(fake.Consent(authURL))
			if rec.Code != http.StatusOK {
				t.Fatalf("callback %d %s", rec.Code, rec.Body.String())
			}
			var connected connectedBody
			if err := json.Unmarshal(rec.Body.Bytes(), &connected); err != nil || connected.AccountID == "" {
				t.Fatalf("body %s", rec.Body.String())
			}
			wantEmail := map[string]string{"google": "flow@gmail.com", "microsoft": "flow@outlook.com"}[p]
			if connected.Provider != p || connected.Email != wantEmail {
				t.Fatalf("callback = %+v, want provider %s and email %s", connected, p, wantEmail)
			}
			if f.stateCount(state) != 0 {
				t.Fatal("the oauth_states row must be gone (one-time use)")
			}

			stored := f.row(connected.AccountID)
			if stored.Status != "active" || !stored.HasTokens || stored.Email != wantEmail {
				t.Fatalf("stored = %+v", stored)
			}
			if bytes.Contains(stored.AccessEnc, []byte("at-")) || bytes.Contains(stored.RefreshEnc, []byte("rt-")) {
				t.Fatal("plaintext tokens must not appear in the database")
			}
			aad := func(column string) []byte {
				return vault.AAD("oauth_tokens", f.tenant.ID, p, fake.AccountID(), column)
			}
			if _, err := f.vlt.Decrypt(stored.AccessEnc, stored.AccessNonce, aad("access_token")); err != nil {
				t.Fatalf("decrypt with the right AAD: %v", err)
			}
			if _, err := f.vlt.Decrypt(stored.AccessEnc, stored.AccessNonce, aad("refresh_token")); err == nil {
				t.Fatal("a ciphertext must not decrypt under another column's AAD")
			}

			rec = f.events(connected.AccountID, "max_results=5")
			if rec.Code != http.StatusOK {
				t.Fatalf("events %d %s", rec.Code, rec.Body.String())
			}
			var page eventsPage
			_ = json.Unmarshal(rec.Body.Bytes(), &page)
			if len(page.Events) != 1 || page.Events[0].Title != "Flow Meeting" || page.Events[0].Provider != p {
				t.Fatalf("events = %+v", page.Events)
			}
			if p == "microsoft" && page.Events[0].Timezone != "America/Bogota" {
				t.Fatalf("Windows zone not mapped: %q", page.Events[0].Timezone)
			}
			if calls := fake.Calls(); calls.Exchange != 1 || calls.Refresh != 0 || calls.Events != 1 {
				t.Fatalf("calls = %+v", calls)
			}

			// Another tenant cannot see the account.
			otherKey := apikey.Generate(apikey.EnvTest)
			other, err := f.tenants.Create(f.ctx, "Other "+p, otherKey.Hash, otherKey.KeyPrefix)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", other.ID) })
			rec = f.do(http.MethodGet, "/v1/calendars/"+connected.AccountID+"/events",
				map[string]string{"Authorization": "Bearer " + otherKey.Plaintext})
			if !hasError(rec, http.StatusNotFound, "not_found") {
				t.Fatalf("other tenant: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestFlowInvalidState(t *testing.T) {
	for _, p := range providers {
		t.Run(p, func(t *testing.T) {
			t.Run("invented", func(t *testing.T) {
				f := newFlow(t)
				rec := f.callback(f.redirect(p) + "?state=nope&code=x")
				if !hasError(rec, http.StatusBadRequest, "invalid_state") {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
				if f.fake(p).Calls().Exchange != 0 {
					t.Fatal("must not reach the token endpoint")
				}
			})

			t.Run("replay", func(t *testing.T) {
				f := newFlow(t)
				cb := f.fake(p).Consent(f.connect(p))
				if rec := f.callback(cb); rec.Code != http.StatusOK {
					t.Fatalf("first %d %s", rec.Code, rec.Body.String())
				}
				if rec := f.callback(cb); !hasError(rec, http.StatusBadRequest, "invalid_state") {
					t.Fatalf("replay %d %s", rec.Code, rec.Body.String())
				}
				if n := f.fake(p).Calls().Exchange; n != 1 {
					t.Fatalf("exchanges = %d, want 1", n)
				}
			})

			t.Run("expired", func(t *testing.T) {
				f := newFlow(t)
				authURL := f.connect(p)
				state := mustURLQuery(t, authURL, "state")
				if _, err := f.pool.Exec(f.ctx,
					`UPDATE oauth_states SET expires_at = now() - interval '1 minute' WHERE state = $1`, state); err != nil {
					t.Fatal(err)
				}
				if rec := f.callback(f.fake(p).Consent(authURL)); !hasError(rec, http.StatusBadRequest, "invalid_state") {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
				if f.stateCount(state) != 0 {
					t.Fatal("an expired state must still be deleted")
				}
			})

			t.Run("denied", func(t *testing.T) {
				f := newFlow(t)
				cb := f.fake(p).Deny(f.connect(p))
				if rec := f.callback(cb); !hasError(rec, http.StatusBadRequest, "access_denied") {
					t.Fatalf("%d %s", rec.Code, rec.Body.String())
				}
				// The denial consumed the state.
				if rec := f.callback(cb); !hasError(rec, http.StatusBadRequest, "invalid_state") {
					t.Fatalf("reuse %d %s", rec.Code, rec.Body.String())
				}
			})

			t.Run("missing params", func(t *testing.T) {
				f := newFlow(t)
				for _, q := range []string{"?code=c", "?state=s"} {
					if rec := f.callback(f.redirect(p) + q); !hasError(rec, http.StatusBadRequest, "invalid_request") {
						t.Fatalf("%s → %d %s", q, rec.Code, rec.Body.String())
					}
				}
			})
		})
	}
}

func TestFlowExpiredTokenRefresh(t *testing.T) {
	for _, p := range providers {
		t.Run(p, func(t *testing.T) {
			f := newFlow(t)
			fake := f.fake(p)
			accountID := f.connectThrough(p)
			before := f.row(accountID)

			fake.ExpireAccessTokens()
			f.expireInDB(accountID)
			if rec := f.events(accountID, "max_results=5"); rec.Code != http.StatusOK {
				t.Fatalf("events %d %s", rec.Code, rec.Body.String())
			}
			if n := fake.Calls().Refresh; n != 1 {
				t.Fatalf("refreshes = %d, want 1", n)
			}
			after := f.row(accountID)
			if !after.ExpiresAt.After(time.Now()) || bytes.Equal(before.AccessEnc, after.AccessEnc) {
				t.Fatal("a new access token with a future expiry must be stored")
			}
			// Still valid: no second refresh.
			if rec := f.events(accountID, "max_results=5"); rec.Code != http.StatusOK || fake.Calls().Refresh != 1 {
				t.Fatalf("second events %d, refreshes %d", rec.Code, fake.Calls().Refresh)
			}

			// The provider rejects an access token Relay thought valid:
			// one forced refresh and one retry.
			f2 := newFlow(t)
			id2 := f2.connectThrough(p)
			f2.fake(p).RevokeAccess()
			b := f2.fake(p).Calls()
			if rec := f2.events(id2, ""); rec.Code != http.StatusOK {
				t.Fatalf("forced refresh events %d %s", rec.Code, rec.Body.String())
			}
			a := f2.fake(p).Calls()
			if a.Refresh-b.Refresh != 1 || a.Events-b.Events != 2 {
				t.Fatalf("refresh delta %d, events delta %d; want 1 and 2", a.Refresh-b.Refresh, a.Events-b.Events)
			}
		})
	}
}

func TestFlowRevokedRefresh(t *testing.T) {
	for _, p := range providers {
		t.Run(p, func(t *testing.T) {
			f := newFlow(t)
			fake := f.fake(p)
			accountID := f.connectThrough(p)
			fake.RevokeAll()
			f.expireInDB(accountID)

			if rec := f.events(accountID, ""); !hasError(rec, http.StatusConflict, "account_revoked") {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if stored := f.row(accountID); stored.Status != "revoked" || stored.HasTokens {
				t.Fatalf("stored = %+v", stored)
			}
			before := fake.Calls().Refresh
			if rec := f.events(accountID, ""); rec.Code != http.StatusConflict || fake.Calls().Refresh != before {
				t.Fatal("an already revoked account must not call the provider again")
			}

			// Reconnecting the same provider account reactivates the same row.
			if id := f.connectThrough(p); id != accountID {
				t.Fatalf("reconnect account_id = %s, want %s", id, accountID)
			}
			if rec := f.events(accountID, ""); rec.Code != http.StatusOK {
				t.Fatalf("events after reconnect %d %s", rec.Code, rec.Body.String())
			}

			// A provider outage during refresh is not a revocation.
			f3 := newFlow(t)
			id3 := f3.connectThrough(p)
			f3.expireInDB(id3)
			f3.fake(p).FailNext("token", http.StatusInternalServerError)
			if rec := f3.events(id3, ""); !hasError(rec, http.StatusBadGateway, "provider_unavailable") {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if f3.row(id3).Status != "active" {
				t.Fatal("a provider 5xx must not revoke the account")
			}
		})
	}
}
