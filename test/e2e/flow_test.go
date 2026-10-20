// Package e2e runs the whole Relay API end to end, as a client would: real
// router, real services, real Postgres, and a stateful fake Google. It only
// uses exported APIs, so it breaks when the public contract breaks.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/httpapi"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/testutil/fakegoogle"
	"github.com/sebasBetancourt/relay/internal/vault"
)

type flow struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	tenants  *store.Tenants
	accounts *store.Accounts
	vlt      *vault.Vault
	google   *fakegoogle.Server
	router   http.Handler
	key      apikey.Key
	tenant   store.Tenant
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	keyBytes := make([]byte, vault.KeySize)
	for i := range keyBytes {
		keyBytes[i] = byte(i + 21)
	}
	vlt, err := vault.New(keyBytes)
	if err != nil {
		t.Fatal(err)
	}

	redirect := "http://localhost:8080/v1/oauth/google/callback"
	// Unique sub per harness so parallel/sub-flows do not hit ErrAccountTaken.
	sub := "sub-flow-" + sanitizeName(t.Name()) + "-" + apikey.Generate(apikey.EnvTest).KeyPrefix
	g := fakegoogle.New(t, fakegoogle.Options{
		ClientID: "flow-client", ClientSecret: "flow-secret",
		RedirectURL: redirect, Sub: sub, AccessTTL: time.Hour,
	})
	g.SetEvents(fakegoogle.Event{
		ID: "ev-flow", Summary: "Flow Meeting",
		Start:    time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339),
		End:      time.Now().UTC().Add(3 * time.Hour).Format(time.RFC3339),
		TimeZone: "UTC",
	})

	base := oauth.GoogleSpec("flow-client", "flow-secret", redirect)
	spec := oauth.ProviderSpec{
		OAuth: &oauth2.Config{
			ClientID: "flow-client", ClientSecret: "flow-secret", RedirectURL: redirect,
			Endpoint: oauth2.Endpoint{AuthURL: g.AuthURL(), TokenURL: g.TokenURL()},
			Scopes:   base.OAuth.Scopes,
		},
		AuthOptions:   base.AuthOptions,
		RequiredScope: base.RequiredScope,
		Identify:      base.Identify,
	}

	tenants := store.NewTenants(pool)
	accounts := store.NewAccounts(pool)
	key := apikey.Generate(apikey.EnvTest)
	tenant, err := tenants.Create(ctx, "Flow "+sanitizeName(t.Name()), key.Hash, key.KeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID)
	})

	httpClient := g.Client()
	httpClient.Timeout = 5 * time.Second
	oauthSvc := oauth.NewService(oauth.Deps{
		States: store.NewOAuthStates(pool), Accounts: accounts, Vault: vlt,
		Providers:  map[oauth.Provider]oauth.ProviderSpec{oauth.ProviderGoogle: spec},
		HTTPClient: httpClient,
		Log:        discardLogger(),
	})
	calSvc := calendar.NewService(oauthSvc, map[oauth.Provider]calendar.Source{
		oauth.ProviderGoogle: &calendar.GoogleSource{BaseURL: g.APIBaseURL(), HTTP: httpClient},
	})

	return &flow{
		t: t, ctx: ctx, pool: pool, tenants: tenants, accounts: accounts,
		vlt: vlt, google: g, key: key, tenant: tenant,
		router: httpapi.NewRouter(httpapi.Deps{
			Log: discardLogger(), DB: pool, Tenants: tenants, Env: apikey.EnvTest,
			Connect: oauthSvc, Callback: oauthSvc, Calendar: calSvc,
		}),
	}
}

// eventsPage is the public JSON contract of GET /v1/calendars/{id}/events,
// declared here on purpose instead of reusing the handler's private type.
type eventsPage struct {
	Events        []calendar.Event `json:"events"`
	NextPageToken string           `json:"next_page_token,omitempty"`
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sanitizeName(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b = append(b, c)
		} else {
			b = append(b, '-')
		}
	}
	if len(b) > 40 {
		b = b[:40]
	}
	return string(b)
}

func (f *flow) do(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func (f *flow) authHdr() map[string]string {
	return map[string]string{"Authorization": "Bearer " + f.key.Plaintext}
}

func (f *flow) connect() string {
	f.t.Helper()
	rec := f.do(http.MethodGet, "/v1/connect/google", f.authHdr())
	if rec.Code != http.StatusFound {
		f.t.Fatalf("connect status %d body %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		f.t.Fatal("missing Location")
	}
	return loc
}

func (f *flow) callback(callbackURL string) *httptest.ResponseRecorder {
	f.t.Helper()
	u, err := url.Parse(callbackURL)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.do(http.MethodGet, u.RequestURI(), nil)
}

func (f *flow) events(accountID, query string) *httptest.ResponseRecorder {
	f.t.Helper()
	path := "/v1/calendars/" + accountID + "/events"
	if query != "" {
		path += "?" + query
	}
	return f.do(http.MethodGet, path, f.authHdr())
}

func (f *flow) connectThrough() string {
	f.t.Helper()
	authURL := f.connect()
	rec := f.callback(f.google.Consent(authURL))
	if rec.Code != 200 {
		f.t.Fatalf("callback %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.AccountID == "" {
		f.t.Fatalf("body %s", rec.Body.String())
	}
	return body.AccountID
}

func (f *flow) expireInDB(accountID string) {
	f.t.Helper()
	_, err := f.pool.Exec(f.ctx, `UPDATE oauth_tokens SET expires_at = now() - interval '1 minute' WHERE account_id = $1`, accountID)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *flow) row(accountID string) store.StoredAccount {
	f.t.Helper()
	a, err := f.accounts.AccountForTenant(f.ctx, accountID, f.tenant.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *flow) stateCount(state string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM oauth_states WHERE state = $1`, state).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func mustURLQuery(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	v := u.Query().Get(key)
	if v == "" {
		t.Fatalf("missing %s in %s", key, raw)
	}
	return v
}

func TestFlowHappyPath(t *testing.T) {
	f := newFlow(t)
	authURL := f.connect()
	state := mustURLQuery(t, authURL, "state")
	rec := f.callback(f.google.Consent(authURL))
	if rec.Code != 200 {
		t.Fatalf("callback %d %s", rec.Code, rec.Body.String())
	}
	var connected struct {
		Status    string `json:"status"`
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &connected); err != nil || connected.AccountID == "" {
		t.Fatalf("body %s", rec.Body.String())
	}
	if f.stateCount(state) != 0 {
		t.Fatal("oauth_states row must be gone")
	}

	stored := f.row(connected.AccountID)
	if stored.Status != "active" || !stored.HasTokens {
		t.Fatalf("stored = %+v", stored)
	}
	if bytes.Contains(stored.AccessEnc, []byte("at-")) {
		t.Fatal("plaintext must not appear in DB")
	}
	plain, err := f.vlt.Decrypt(stored.AccessEnc, stored.AccessNonce,
		vault.AAD("oauth_tokens", f.tenant.ID, "google", f.google.Sub(), "access_token"))
	if err != nil || len(plain) == 0 {
		t.Fatalf("decrypt: %v", err)
	}
	if _, err := f.vlt.Decrypt(stored.AccessEnc, stored.AccessNonce,
		vault.AAD("oauth_tokens", f.tenant.ID, "google", f.google.Sub(), "refresh_token")); err == nil {
		t.Fatal("wrong-column AAD must fail")
	}

	rec = f.events(connected.AccountID, "max_results=5")
	if rec.Code != 200 {
		t.Fatalf("events %d %s", rec.Code, rec.Body.String())
	}
	var page eventsPage
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Events) != 1 || page.Events[0].Title != "Flow Meeting" {
		t.Fatalf("events = %+v", page.Events)
	}
	calls := f.google.Calls()
	if calls.Exchange != 1 || calls.Refresh != 0 || calls.Events != 1 {
		t.Fatalf("calls = %+v", calls)
	}

	otherKey := apikey.Generate(apikey.EnvTest)
	other, err := f.tenants.Create(f.ctx, "OtherFlow", otherKey.Hash, otherKey.KeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", other.ID)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/calendars/"+connected.AccountID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+otherKey.Plaintext)
	orec := httptest.NewRecorder()
	f.router.ServeHTTP(orec, req)
	if orec.Code != 404 {
		t.Fatalf("other tenant status %d", orec.Code)
	}
}

func TestFlowInvalidState(t *testing.T) {
	t.Run("invented", func(t *testing.T) {
		f := newFlow(t)
		rec := f.callback(f.redirect() + "?state=nope&code=x")
		if rec.Code != 400 || !bytes.Contains(rec.Body.Bytes(), []byte(`"invalid_state"`)) {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if f.google.Calls().Exchange != 0 {
			t.Fatal("must not hit token endpoint")
		}
	})

	t.Run("replay", func(t *testing.T) {
		f := newFlow(t)
		cb := f.google.Consent(f.connect())
		rec1 := f.callback(cb)
		if rec1.Code != 200 {
			t.Fatalf("first %d", rec1.Code)
		}
		rec2 := f.callback(cb)
		if rec2.Code != 400 || !bytes.Contains(rec2.Body.Bytes(), []byte(`"invalid_state"`)) {
			t.Fatalf("replay %d %s", rec2.Code, rec2.Body.String())
		}
		if f.google.Calls().Exchange != 1 {
			t.Fatalf("exchange = %d", f.google.Calls().Exchange)
		}
	})

	t.Run("expired", func(t *testing.T) {
		f := newFlow(t)
		authURL := f.connect()
		state := mustURLQuery(t, authURL, "state")
		_, err := f.pool.Exec(f.ctx, `UPDATE oauth_states SET expires_at = now() - interval '1 minute' WHERE state = $1`, state)
		if err != nil {
			t.Fatal(err)
		}
		rec := f.callback(f.google.Consent(authURL))
		if rec.Code != 400 || !bytes.Contains(rec.Body.Bytes(), []byte(`"invalid_state"`)) {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		if f.stateCount(state) != 0 {
			t.Fatal("expired state must be deleted")
		}
	})

	t.Run("denied", func(t *testing.T) {
		f := newFlow(t)
		authURL := f.connect()
		cb := f.google.Deny(authURL)
		rec := f.callback(cb)
		if rec.Code != 400 || !bytes.Contains(rec.Body.Bytes(), []byte(`"access_denied"`)) {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
		// Reuse same callback URL (same state) → already consumed.
		rec2 := f.callback(cb)
		if rec2.Code != 400 || !bytes.Contains(rec2.Body.Bytes(), []byte(`"invalid_state"`)) {
			t.Fatalf("reuse %d %s", rec2.Code, rec2.Body.String())
		}
	})

	t.Run("missing_params", func(t *testing.T) {
		f := newFlow(t)
		for _, q := range []string{"?code=c", "?state=s"} {
			rec := f.callback(f.redirect() + q)
			if rec.Code != 400 || !bytes.Contains(rec.Body.Bytes(), []byte(`"invalid_request"`)) {
				t.Fatalf("%s → %d %s", q, rec.Code, rec.Body.String())
			}
		}
	})
}

func (f *flow) redirect() string {
	return "http://localhost:8080/v1/oauth/google/callback"
}

func TestFlowExpiredTokenRefresh(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough()
	before := f.row(accountID)

	f.google.ExpireAccessTokens()
	f.expireInDB(accountID)

	rec := f.events(accountID, "max_results=5")
	if rec.Code != 200 {
		t.Fatalf("events %d %s", rec.Code, rec.Body.String())
	}
	if f.google.Calls().Refresh != 1 {
		t.Fatalf("refresh = %d", f.google.Calls().Refresh)
	}
	after := f.row(accountID)
	if !after.ExpiresAt.After(time.Now()) {
		t.Fatal("expires_at must be in the future")
	}
	if bytes.Equal(before.AccessEnc, after.AccessEnc) {
		t.Fatal("access ciphertext must change after refresh")
	}

	rec = f.events(accountID, "max_results=5")
	if rec.Code != 200 {
		t.Fatalf("second events %d", rec.Code)
	}
	if f.google.Calls().Refresh != 1 {
		t.Fatalf("refresh must stay 1, got %d", f.google.Calls().Refresh)
	}

	// Variant: Google rejects access we thought was valid → forced refresh + retry.
	f2 := newFlow(t)
	id2 := f2.connectThrough()
	f2.google.RevokeAccess()
	beforeCalls := f2.google.Calls()
	rec = f2.events(id2, "")
	if rec.Code != 200 {
		t.Fatalf("forced refresh events %d %s", rec.Code, rec.Body.String())
	}
	afterCalls := f2.google.Calls()
	if afterCalls.Refresh-beforeCalls.Refresh != 1 {
		t.Fatalf("refresh delta = %d", afterCalls.Refresh-beforeCalls.Refresh)
	}
	if afterCalls.Events-beforeCalls.Events != 2 {
		t.Fatalf("events delta = %d want 2", afterCalls.Events-beforeCalls.Events)
	}
}

func TestFlowRevokedRefresh(t *testing.T) {
	f := newFlow(t)
	accountID := f.connectThrough()
	f.google.RevokeAll()
	f.expireInDB(accountID)

	rec := f.events(accountID, "")
	if rec.Code != 409 || !bytes.Contains(rec.Body.Bytes(), []byte(`"account_revoked"`)) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	stored := f.row(accountID)
	if stored.Status != "revoked" || stored.HasTokens {
		t.Fatalf("stored = %+v", stored)
	}
	refreshBefore := f.google.Calls().Refresh
	rec = f.events(accountID, "")
	if rec.Code != 409 {
		t.Fatalf("second %d", rec.Code)
	}
	if f.google.Calls().Refresh != refreshBefore {
		t.Fatal("must not call Google again when already revoked")
	}

	// Reconnect same Google account → same account_id, active again.
	authURL := f.connect()
	rec = f.callback(f.google.Consent(authURL))
	if rec.Code != 200 {
		t.Fatalf("reconnect callback %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		AccountID string `json:"account_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.AccountID != accountID {
		t.Fatalf("account_id = %s want %s", body.AccountID, accountID)
	}
	if f.row(accountID).Status != "active" {
		t.Fatal("must be active after reconnect")
	}
	rec = f.events(accountID, "")
	if rec.Code != 200 {
		t.Fatalf("events after reconnect %d %s", rec.Code, rec.Body.String())
	}

	// Variant: Google 5xx on refresh → 502, account stays active.
	f3 := newFlow(t)
	id3 := f3.connectThrough()
	f3.expireInDB(id3)
	f3.google.FailNext("token", 500)
	rec = f3.events(id3, "")
	if rec.Code != 502 || !bytes.Contains(rec.Body.Bytes(), []byte(`"provider_unavailable"`)) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if f3.row(id3).Status != "active" {
		t.Fatal("5xx must not revoke")
	}
}
