package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/vault"
)

const msRedirect = "http://localhost:8080/v1/oauth/microsoft/callback"

func newMicrosoftStartService(states StateStore, tenant string) *Service {
	return NewService(Deps{
		States: states,
		Providers: map[Provider]ProviderSpec{
			ProviderMicrosoft: MicrosoftSpec("ms-client", "ms-secret", msRedirect, tenant, MicrosoftGraphURL),
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
}

func TestStartMicrosoft(t *testing.T) {
	states := &fakeStates{}
	svc := newMicrosoftStartService(states, "common")

	raw, err := svc.Start(context.Background(), "tenant-1", ProviderMicrosoft)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(states.saved) != 1 {
		t.Fatalf("saved %d states, want 1", len(states.saved))
	}
	saved := states.saved[0]
	if saved.Provider != "microsoft" || saved.TenantID != "tenant-1" || states.ttls[0] != StateTTL {
		t.Fatalf("saved %+v ttl %v", saved, states.ttls[0])
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "login.microsoftonline.com" || u.Path != "/common/oauth2/v2.0/authorize" {
		t.Fatalf("authorize URL = %s://%s%s", u.Scheme, u.Host, u.Path)
	}

	sum := sha256.Sum256([]byte(saved.CodeVerifier))
	q := u.Query()
	want := map[string]string{
		"state":                 saved.State,
		"code_challenge":        base64.RawURLEncoding.EncodeToString(sum[:]),
		"code_challenge_method": "S256",
		"client_id":             "ms-client",
		"redirect_uri":          msRedirect,
		"response_type":         "code",
		"prompt":                "select_account",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	scopes := strings.Fields(q.Get("scope"))
	slices.Sort(scopes)
	wantScopes := []string{"Calendars.Read", "User.Read", "email", "offline_access", "openid"}
	if !slices.Equal(scopes, wantScopes) {
		t.Errorf("scopes = %v, want %v", scopes, wantScopes)
	}
	// Google-only parameters and secrets must not leak into the Microsoft URL.
	for _, k := range []string{"access_type", "client_secret", "code_verifier"} {
		if q.Has(k) {
			t.Errorf("authorize URL must not carry %s", k)
		}
	}
}

func TestStartMicrosoftTenantInPath(t *testing.T) {
	const guid = "72f988bf-86f1-41af-91ab-2d7cd011db47"
	svc := newMicrosoftStartService(&fakeStates{}, guid)

	raw, err := svc.Start(context.Background(), "tenant-1", ProviderMicrosoft)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	if u.Path != "/"+guid+"/oauth2/v2.0/authorize" {
		t.Fatalf("path = %q", u.Path)
	}
}

func TestMicrosoftConfigSendsSecretInBody(t *testing.T) {
	cfg := MicrosoftConfig("id", "secret", msRedirect, "common")
	if cfg.Endpoint.AuthStyle != oauth2.AuthStyleInParams {
		t.Fatalf("auth style = %v, want AuthStyleInParams", cfg.Endpoint.AuthStyle)
	}
	if cfg.Endpoint.TokenURL != "https://login.microsoftonline.com/common/oauth2/v2.0/token" {
		t.Fatalf("token URL = %q", cfg.Endpoint.TokenURL)
	}
}

func TestMicrosoftHasScope(t *testing.T) {
	has := microsoftHasScope(MicrosoftCalendarsRead)
	tests := []struct {
		granted []string
		want    bool
	}{
		{[]string{"Calendars.Read"}, true},
		{[]string{"https://graph.microsoft.com/Calendars.Read"}, true},
		{[]string{"HTTPS://GRAPH.MICROSOFT.COM/calendars.read"}, true},
		{[]string{"openid", "User.Read", "Calendars.Read", "offline_access"}, true},
		{[]string{"User.Read", "offline_access"}, false},
		{[]string{"Calendars.ReadWrite.Shared"}, false},
		{[]string{"https://outlook.office.com/Calendars.Read"}, false}, // another resource's token
		{nil, false},
	}
	for _, tt := range tests {
		if got := has(tt.granted); got != tt.want {
			t.Errorf("HasScope(%v) = %v, want %v", tt.granted, got, tt.want)
		}
	}
}

// fakeGraphMe serves /v1.0/me and records the request it got.
func fakeGraphMe(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestMicrosoftIdentify(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		want      Identity
		wantErrIs error
	}{
		{"work account with mail", 200, `{"id":"abc-123","mail":"ana@contoso.com","userPrincipalName":"ana@contoso.onmicrosoft.com"}`,
			Identity{AccountID: "abc-123", Email: "ana@contoso.com"}, nil},
		{"personal account falls back to UPN", 200, `{"id":"0123abcd","mail":null,"userPrincipalName":"ana@outlook.com"}`,
			Identity{AccountID: "0123abcd", Email: "ana@outlook.com"}, nil},
		{"no email at all", 200, `{"id":"0123abcd"}`, Identity{AccountID: "0123abcd"}, nil},
		{"missing id", 200, `{"mail":"ana@contoso.com"}`, Identity{}, ErrIdentityLookup},
		{"not json", 200, `<html>`, Identity{}, ErrIdentityLookup},
		{"token rejected", 401, `{"error":{"code":"InvalidAuthenticationToken"}}`, Identity{}, ErrIdentityLookup},
		{"forbidden", 403, `{}`, Identity{}, ErrIdentityLookup},
		{"graph down", 503, `{}`, Identity{}, ErrProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, req := fakeGraphMe(t, tt.status, tt.body)
			identify := microsoftIdentify(srv.URL + "/")

			got, err := identify(context.Background(), srv.Client(), &oauth2.Token{AccessToken: "graph-access"}, "ms-client")
			if tt.wantErrIs != nil {
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("err = %v, want %v", err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("identity = %+v, want %+v", got, tt.want)
			}
			if req.URL.Path != "/v1.0/me" || req.URL.Query().Get("$select") != "id,mail,userPrincipalName" {
				t.Fatalf("request = %s", req.URL)
			}
			if req.Header.Get("Authorization") != "Bearer graph-access" {
				t.Fatalf("authorization = %q", req.Header.Get("Authorization"))
			}
		})
	}
}

func TestMicrosoftIdentifyNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // connection refused from now on

	_, err := microsoftIdentify(base)(context.Background(), &http.Client{Timeout: time.Second}, &oauth2.Token{AccessToken: "x"}, "")
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
}

// fakeMicrosoft is a minimal Entra ID token endpoint + Graph /me.
type fakeMicrosoft struct {
	srv       *httptest.Server
	tokenForm url.Values
	tokenAuth string
	scope     string
}

func startFakeMicrosoft(t *testing.T, scope string) *fakeMicrosoft {
	t.Helper()
	f := &fakeMicrosoft{scope: scope}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /common/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.tokenForm = r.PostForm
		f.tokenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "ms-access",
			"refresh_token": "ms-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         f.scope,
		})
	})
	mux.HandleFunc("GET /v1.0/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ms-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"id":"ms-user-1","mail":null,"userPrincipalName":"ana@outlook.com"}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMicrosoft) spec() ProviderSpec {
	spec := MicrosoftSpec("ms-client", "ms-secret", msRedirect, "common", f.srv.URL)
	spec.OAuth.Endpoint.AuthURL = f.srv.URL + "/common/oauth2/v2.0/authorize"
	spec.OAuth.Endpoint.TokenURL = f.srv.URL + "/common/oauth2/v2.0/token"
	return spec
}

func newMicrosoftFinishService(t *testing.T, states StateStore, accounts AccountStore, spec ProviderSpec) (*Service, *vault.Vault) {
	t.Helper()
	key := make([]byte, vault.KeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	vlt, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(Deps{
		States:     states,
		Accounts:   accounts,
		Vault:      vlt,
		Providers:  map[Provider]ProviderSpec{ProviderMicrosoft: spec},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}), vlt
}

func TestFinishMicrosoft(t *testing.T) {
	ms := startFakeMicrosoft(t, "https://graph.microsoft.com/Calendars.Read https://graph.microsoft.com/User.Read openid email offline_access")
	states := &fakeStates{}
	accounts := &fakeAccounts{}
	svc, vlt := newMicrosoftFinishService(t, states, accounts, ms.spec())

	if _, err := svc.Start(context.Background(), "tenant-1", ProviderMicrosoft); err != nil {
		t.Fatal(err)
	}
	st := states.saved[0]

	res, err := svc.Finish(context.Background(), ProviderMicrosoft, st.State, "auth-code", "")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if res.Provider != ProviderMicrosoft {
		t.Fatalf("result = %+v", res)
	}

	// Token request: PKCE verifier and client secret in the form body
	// (AuthStyleInParams), no Basic header, no state.
	form := ms.tokenForm
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "auth-code" {
		t.Fatalf("token form = %v", form)
	}
	if form.Get("code_verifier") != st.CodeVerifier {
		t.Fatal("code_verifier not sent")
	}
	if form.Get("client_secret") != "ms-secret" || ms.tokenAuth != "" {
		t.Fatalf("client secret must travel in the body only (auth header %q)", ms.tokenAuth)
	}
	if form.Has("state") {
		t.Fatal("state must not be sent to the token endpoint")
	}

	if len(accounts.saved) != 1 {
		t.Fatalf("saved %d connections", len(accounts.saved))
	}
	c := accounts.saved[0]
	if c.Provider != "microsoft" || c.ProviderAccountID != "ms-user-1" || c.TenantID != "tenant-1" {
		t.Fatalf("connection = %+v", c)
	}
	plain, err := vlt.Decrypt(c.RefreshEnc, c.RefreshNonce, tokenAAD("tenant-1", "microsoft", "ms-user-1", "refresh_token"))
	if err != nil || string(plain) != "ms-refresh" {
		t.Fatalf("refresh token not stored encrypted under the Graph id: %q %v", plain, err)
	}
}

func TestFinishMicrosoftWithoutCalendarScope(t *testing.T) {
	ms := startFakeMicrosoft(t, "User.Read openid offline_access")
	states := &fakeStates{}
	accounts := &fakeAccounts{}
	svc, _ := newMicrosoftFinishService(t, states, accounts, ms.spec())

	if _, err := svc.Start(context.Background(), "tenant-1", ProviderMicrosoft); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Finish(context.Background(), ProviderMicrosoft, states.saved[0].State, "auth-code", "")
	if !errors.Is(err, ErrInsufficientScope) {
		t.Fatalf("err = %v, want ErrInsufficientScope", err)
	}
	if len(accounts.saved) != 0 {
		t.Fatal("nothing may be saved without Calendars.Read")
	}
}

func TestGoogleIdentifyEmail(t *testing.T) {
	idToken := func(claims map[string]any) string {
		claims["iss"] = "https://accounts.google.com"
		claims["aud"] = "client-123"
		claims["sub"] = "google-sub"
		claims["exp"] = time.Now().Add(time.Hour).Unix()
		payload, _ := json.Marshal(claims)
		return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
	}
	tests := []struct {
		name      string
		claims    map[string]any
		wantEmail string
	}{
		{"verified", map[string]any{"email": "ana@gmail.com", "email_verified": true}, "ana@gmail.com"},
		{"unverified is dropped", map[string]any{"email": "ana@gmail.com", "email_verified": false}, ""},
		{"no email scope", map[string]any{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok := (&oauth2.Token{AccessToken: "a"}).WithExtra(map[string]any{"id_token": idToken(tt.claims)})
			got, err := googleIdentify(context.Background(), nil, tok, "client-123")
			if err != nil {
				t.Fatal(err)
			}
			if got.AccountID != "google-sub" || got.Email != tt.wantEmail {
				t.Fatalf("identity = %+v, want email %q", got, tt.wantEmail)
			}
		})
	}
}
