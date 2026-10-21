// Package e2e runs the whole Relay API end to end, as a client would: real
// router, real services, real Postgres, and stateful fake providers (Google
// and Microsoft). It only uses exported APIs, so it breaks when the public
// contract breaks.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
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
	"github.com/sebasBetancourt/relay/internal/testutil/fakemicrosoft"
	"github.com/sebasBetancourt/relay/internal/testutil/fakeoauth"
	"github.com/sebasBetancourt/relay/internal/vault"
)

// providers every shared scenario runs for.
var providers = []string{"google", "microsoft"}

// fakeProvider is what scenarios need from either fake.
type fakeProvider interface {
	Consent(authURL string) string
	Deny(authURL string) string
	ExpireAccessTokens()
	RevokeAccess()
	RevokeAll()
	FailNext(endpoint string, status int)
	FailNextRefresh(code string)
	RefreshTokenValid(token string) bool
	Calls() fakeoauth.Calls
	AccountID() string
}

const (
	googleRedirect    = "http://localhost:8080/v1/oauth/google/callback"
	microsoftRedirect = "http://localhost:8080/v1/oauth/microsoft/callback"
)

type flow struct {
	t        *testing.T
	ctx      context.Context
	dbURL    string
	pool     *pgxpool.Pool
	tenants  *store.Tenants
	accounts *store.Accounts
	vlt      *vault.Vault
	google   *fakegoogle.Server
	ms       *fakemicrosoft.Server
	router   http.Handler
	key      apikey.Key
	tenant   store.Tenant
}

// newFlow wires one API "replica" with both providers enabled, a fresh
// tenant and fresh fakes. Account ids are unique per test so parallel
// packages never collide on (provider, provider_account_id).
func newFlow(t *testing.T) *flow {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	pool := newPool(t, dbURL)

	keyBytes := make([]byte, vault.KeySize)
	for i := range keyBytes {
		keyBytes[i] = byte(i + 21)
	}
	vlt, err := vault.New(keyBytes)
	if err != nil {
		t.Fatal(err)
	}

	unique := sanitizeName(t.Name()) + "-" + apikey.Generate(apikey.EnvTest).KeyPrefix
	soon := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	g := fakegoogle.New(t, fakegoogle.Options{
		ClientID: "flow-client", ClientSecret: "flow-secret", RedirectURL: googleRedirect,
		Sub: "g-" + unique, Email: "flow@gmail.com",
	})
	g.SetEvents(fakegoogle.Event{
		ID: "ev-flow", Summary: "Flow Meeting",
		Start: soon.Format(time.RFC3339), End: soon.Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC",
	})
	ms := fakemicrosoft.New(t, fakemicrosoft.Options{
		ClientID: "flow-ms-client", ClientSecret: "flow-ms-secret", RedirectURL: microsoftRedirect,
		UserID: "ms-" + unique, Email: "flow@outlook.com",
	})
	ms.SetEvents(fakemicrosoft.Event{
		ID: "ev-flow", Subject: "Flow Meeting", Start: soon, End: soon.Add(time.Hour),
		TimeZone: "SA Pacific Standard Time",
	})

	tenants := store.NewTenants(pool)
	key := apikey.Generate(apikey.EnvTest)
	tenant, err := tenants.Create(ctx, "Flow "+sanitizeName(t.Name()), key.Hash, key.KeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID)
	})

	f := &flow{
		t: t, ctx: ctx, dbURL: dbURL, pool: pool, tenants: tenants, accounts: store.NewAccounts(pool),
		vlt: vlt, google: g, ms: ms, key: key, tenant: tenant,
	}
	f.router = f.buildRouter(pool)
	return f
}

func newPool(t *testing.T, dbURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := store.NewPool(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// buildRouter assembles services and router over pool, as cmd/api does.
func (f *flow) buildRouter(pool *pgxpool.Pool) http.Handler {
	client := &http.Client{Timeout: 5 * time.Second}

	gBase := oauth.GoogleSpec("flow-client", "flow-secret", googleRedirect)
	gSpec := oauth.ProviderSpec{
		OAuth: &oauth2.Config{
			ClientID: "flow-client", ClientSecret: "flow-secret", RedirectURL: googleRedirect,
			Endpoint: oauth2.Endpoint{AuthURL: f.google.AuthURL(), TokenURL: f.google.TokenURL()},
			Scopes:   gBase.OAuth.Scopes,
		},
		AuthOptions:   gBase.AuthOptions,
		RequiredScope: gBase.RequiredScope,
		Identify:      gBase.Identify,
		Revoke:        oauth.GoogleRevoke(f.google.RevokeURL()), // never the real Google
	}
	msSpec := oauth.MicrosoftSpec("flow-ms-client", "flow-ms-secret", microsoftRedirect, "common", f.ms.GraphBaseURL())
	msSpec.OAuth.Endpoint.AuthURL = f.ms.AuthURL()
	msSpec.OAuth.Endpoint.TokenURL = f.ms.TokenURL()

	accounts := store.NewAccounts(pool)
	oauthSvc := oauth.NewService(oauth.Deps{
		States: store.NewOAuthStates(pool), Accounts: accounts, Vault: f.vlt,
		Providers: map[oauth.Provider]oauth.ProviderSpec{
			oauth.ProviderGoogle:    gSpec,
			oauth.ProviderMicrosoft: msSpec,
		},
		HTTPClient: client,
		Log:        discardLogger(),
	})
	calSvc := calendar.NewService(oauthSvc, map[oauth.Provider]calendar.Source{
		oauth.ProviderGoogle:    &calendar.GoogleSource{BaseURL: f.google.APIBaseURL(), HTTP: client},
		oauth.ProviderMicrosoft: &calendar.MicrosoftSource{BaseURL: f.ms.GraphBaseURL(), HTTP: client},
	})
	return httpapi.NewRouter(httpapi.Deps{
		Log: discardLogger(), DB: pool, Tenants: store.NewTenants(pool), Env: apikey.EnvTest,
		Connect: oauthSvc, Callback: oauthSvc, Calendar: calSvc,
		Accounts: accounts, Disconnect: oauthSvc,
	})
}

// replica is a second API instance: its own pool, services and caches,
// sharing only Postgres and the fakes, like another pod.
func (f *flow) replica() http.Handler {
	return f.buildRouter(newPool(f.t, f.dbURL))
}

func (f *flow) fake(provider string) fakeProvider {
	if provider == "microsoft" {
		return f.ms
	}
	return f.google
}

func (f *flow) redirect(provider string) string {
	if provider == "microsoft" {
		return microsoftRedirect
	}
	return googleRedirect
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
		b = b[len(b)-40:]
	}
	return string(b)
}

func doOn(router http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func (f *flow) do(method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	return doOn(f.router, method, path, hdr)
}

func (f *flow) authHdr() map[string]string {
	return map[string]string{"Authorization": "Bearer " + f.key.Plaintext}
}

func (f *flow) connect(provider string) string {
	f.t.Helper()
	rec := f.do(http.MethodGet, "/v1/connect/"+provider, f.authHdr())
	if rec.Code != http.StatusFound {
		f.t.Fatalf("connect %s: status %d body %s", provider, rec.Code, rec.Body.String())
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

type connectedBody struct {
	Status    string `json:"status"`
	Provider  string `json:"provider"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
}

// connectThrough runs connect → consent → callback and returns account_id.
func (f *flow) connectThrough(provider string) string {
	f.t.Helper()
	rec := f.callback(f.fake(provider).Consent(f.connect(provider)))
	if rec.Code != http.StatusOK {
		f.t.Fatalf("callback %s: %d %s", provider, rec.Code, rec.Body.String())
	}
	var body connectedBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.AccountID == "" {
		f.t.Fatalf("callback body %s", rec.Body.String())
	}
	return body.AccountID
}

func (f *flow) events(accountID, query string) *httptest.ResponseRecorder {
	f.t.Helper()
	path := "/v1/calendars/" + accountID + "/events"
	if query != "" {
		path += "?" + query
	}
	return f.do(http.MethodGet, path, f.authHdr())
}

func (f *flow) expireInDB(accountID string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE oauth_tokens SET expires_at = now() - interval '1 minute' WHERE account_id = $1`, accountID); err != nil {
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

// storedRefresh decrypts the refresh token Relay holds for the account.
func (f *flow) storedRefresh(provider, accountID string) string {
	f.t.Helper()
	a := f.row(accountID)
	plain, err := f.vlt.Decrypt(a.RefreshEnc, a.RefreshNonce,
		vault.AAD("oauth_tokens", f.tenant.ID, provider, f.fake(provider).AccountID(), "refresh_token"))
	if err != nil {
		f.t.Fatalf("decrypt stored refresh: %v", err)
	}
	return string(plain)
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

func hasError(rec *httptest.ResponseRecorder, status int, code string) bool {
	return rec.Code == status && strings.Contains(rec.Body.String(), `"`+code+`"`)
}
