package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

// fakeStates records what the real oauth.Service tries to persist.
type fakeStates struct {
	saved []store.OAuthState
	err   error
}

func (f *fakeStates) Create(_ context.Context, st store.OAuthState, _ time.Duration) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, st)
	return nil
}

func (f *fakeStates) Consume(_ context.Context, state, provider string) (store.ConsumedState, error) {
	return store.ConsumedState{}, store.ErrNotFound
}

type connectFixture struct {
	router http.Handler
	states *fakeStates
	key    apikey.Key
	tenant store.Tenant
}

// newConnectFixture wires the real router and the real oauth.Service;
// only the tenant lookup and the state store are fakes.
func newConnectFixture(t *testing.T, storeErr error) connectFixture {
	t.Helper()
	key := apikey.Generate(apikey.EnvTest)
	tenant := store.Tenant{ID: "tenant-42", Name: "Acme", KeyPrefix: key.KeyPrefix}
	states := &fakeStates{err: storeErr}
	svc := oauth.NewService(oauth.Deps{
		States: states,
		Providers: map[oauth.Provider]oauth.ProviderSpec{
			oauth.ProviderGoogle: oauth.GoogleSpec("client-123", "secret-xyz", "http://localhost:8080/v1/oauth/google/callback"),
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	router := NewRouter(Deps{
		Log:      discardLogger(),
		Tenants:  &fakeLookup{tenants: map[string]store.Tenant{key.Hash: tenant}},
		Env:      apikey.EnvTest,
		Connect:  svc,
		Callback: svc,
	})
	return connectFixture{router: router, states: states, key: key, tenant: tenant}
}

func (f connectFixture) get(path, auth, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// assertConsentURL checks the URL points to Google and carries exactly the
// state and PKCE challenge that were saved.
func assertConsentURL(t *testing.T, raw string, saved store.OAuthState) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("bad URL %q: %v", raw, err)
	}
	if u.Host != "accounts.google.com" {
		t.Fatalf("host = %q, want accounts.google.com", u.Host)
	}
	sum := sha256.Sum256([]byte(saved.CodeVerifier))
	q := u.Query()
	if got := q.Get("state"); got != saved.State {
		t.Fatalf("state in URL = %q, saved %q", got, saved.State)
	}
	if got, want := q.Get("code_challenge"), base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
		t.Fatalf("code_challenge = %q, want %q", got, want)
	}
}

func TestConnectGoogleRedirects(t *testing.T) {
	f := newConnectFixture(t, nil)

	rec := f.get("/v1/connect/google", "Bearer "+f.key.Plaintext, "")

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if len(f.states.saved) != 1 {
		t.Fatalf("saved %d states, want 1", len(f.states.saved))
	}
	saved := f.states.saved[0]
	if saved.TenantID != f.tenant.ID || saved.Provider != "google" {
		t.Fatalf("saved %+v, want tenant %s / google", saved, f.tenant.ID)
	}
	assertConsentURL(t, rec.Header().Get("Location"), saved)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestConnectGoogleJSON(t *testing.T) {
	f := newConnectFixture(t, nil)

	rec := f.get("/v1/connect/google", "Bearer "+f.key.Plaintext, "application/json")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("JSON answer must not redirect")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	var body struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	assertConsentURL(t, body.AuthorizationURL, f.states.saved[0])
}

func TestConnectErrors(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		withKey    bool
		storeErr   error
		wantStatus int
		wantBody   string
	}{
		{"no api key", "/v1/connect/google", false, nil, http.StatusUnauthorized, `{"error":"unauthorized"}`},
		{"unknown provider", "/v1/connect/yahoo", true, nil, http.StatusBadRequest, `{"error":"unsupported_provider"}`},
		{"provider not enabled yet", "/v1/connect/microsoft", true, nil, http.StatusBadRequest, `{"error":"unsupported_provider"}`},
		{"tenant deleted, key still cached", "/v1/connect/google", true, store.ErrNotFound, http.StatusUnauthorized, `{"error":"unauthorized"}`},
		{"db failure", "/v1/connect/google", true, errors.New("db down"), http.StatusInternalServerError, `{"error":"internal error"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newConnectFixture(t, tt.storeErr)
			auth := ""
			if tt.withKey {
				auth = "Bearer " + f.key.Plaintext
			}

			rec := f.get(tt.path, auth, "")

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Body.String(); got != tt.wantBody+"\n" {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
			if rec.Header().Get("Location") != "" {
				t.Fatal("errors must not redirect")
			}
			if len(f.states.saved) != 0 {
				t.Fatalf("no state may be saved, got %d", len(f.states.saved))
			}
		})
	}
}

func TestConnectRejectsPost(t *testing.T) {
	f := newConnectFixture(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/connect/google", nil)
	req.Header.Set("Authorization", "Bearer "+f.key.Plaintext)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestConnectMicrosoftRedirects(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	tenant := store.Tenant{ID: "tenant-ms", Name: "Contoso", KeyPrefix: key.KeyPrefix}
	states := &fakeStates{}
	svc := oauth.NewService(oauth.Deps{
		States: states,
		Providers: map[oauth.Provider]oauth.ProviderSpec{
			oauth.ProviderMicrosoft: oauth.MicrosoftSpec("ms-client", "ms-secret",
				"http://localhost:8080/v1/oauth/microsoft/callback", "common", oauth.MicrosoftGraphURL),
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	router := NewRouter(Deps{
		Log:     discardLogger(),
		Tenants: &fakeLookup{tenants: map[string]store.Tenant{key.Hash: tenant}},
		Env:     apikey.EnvTest,
		Connect: svc,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/connect/microsoft", nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
	}
	if len(states.saved) != 1 || states.saved[0].Provider != "microsoft" || states.saved[0].TenantID != tenant.ID {
		t.Fatalf("saved = %+v", states.saved)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Host != "login.microsoftonline.com" || loc.Path != "/common/oauth2/v2.0/authorize" {
		t.Fatalf("Location = %s", loc)
	}
	sum := sha256.Sum256([]byte(states.saved[0].CodeVerifier))
	if loc.Query().Get("state") != states.saved[0].State ||
		loc.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("Location must carry the saved state and its PKCE challenge")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing Cache-Control: no-store")
	}
}
