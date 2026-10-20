package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/vault"
)

type fakeAccounts struct {
	saved []store.Connection
	err   error
	id    string
}

func (f *fakeAccounts) SaveConnection(_ context.Context, c store.Connection) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.saved = append(f.saved, c)
	if f.id == "" {
		return "acct-1", nil
	}
	return f.id, nil
}

func (f *fakeAccounts) AccountForTenant(context.Context, string, string) (store.StoredAccount, error) {
	return store.StoredAccount{}, store.ErrNotFound
}

func (f *fakeAccounts) RefreshLocked(context.Context, string, func(store.StoredAccount) (*store.TokenUpdate, error)) error {
	return store.ErrNotFound
}

func (f *fakeAccounts) MarkRevoked(context.Context, string) error { return nil }

func makeIDToken(t *testing.T, aud, iss, sub string, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]any{
		"iss": iss,
		"aud": aud,
		"sub": sub,
		"exp": exp.Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func startFakeGoogle(t *testing.T, handler http.HandlerFunc) (*httptest.Server, ProviderSpec) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// oauth2.Exchange only parses JSON when Content-Type says so.
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	cfg := &oauth2.Config{
		ClientID:     "client-123",
		ClientSecret: "secret-xyz",
		RedirectURL:  redirect,
		Endpoint: oauth2.Endpoint{
			AuthURL:  srv.URL + "/auth",
			TokenURL: srv.URL + "/token",
		},
		Scopes: []string{GoogleOpenID, GoogleCalendarReadonly},
	}
	return srv, ProviderSpec{
		OAuth:         cfg,
		RequiredScope: GoogleCalendarReadonly,
		Identify:      googleIdentify,
	}
}

func newFinishService(t *testing.T, states StateStore, accounts AccountStore, spec ProviderSpec) (*Service, *vault.Vault) {
	t.Helper()
	key := make([]byte, vault.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	vlt, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(Deps{
		States:     states,
		Accounts:   accounts,
		Vault:      vlt,
		Providers:  map[Provider]ProviderSpec{ProviderGoogle: spec},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}), vlt
}

func TestFinishExchangeSendsPKCEAndNoState(t *testing.T) {
	var (
		gotForm url.Values
		gotAuth string
	)
	_, spec := startFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		gotAuth = r.Header.Get("Authorization")
		idTok := makeIDToken(t, "client-123", "https://accounts.google.com", "sub-99", time.Now().Add(time.Hour))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-plain",
			"refresh_token": "refresh-plain",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         GoogleOpenID + " " + GoogleCalendarReadonly,
			"id_token":      idTok,
		})
	})

	states := &fakeStates{consume: map[string]store.ConsumedState{
		"google:st1": {TenantID: "tenant-1", CodeVerifier: "verifier-abc"},
	}}
	accounts := &fakeAccounts{}
	svc, vlt := newFinishService(t, states, accounts, spec)

	res, err := svc.Finish(context.Background(), ProviderGoogle, "st1", "auth-code", "")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if res.AccountID != "acct-1" || res.Provider != ProviderGoogle {
		t.Fatalf("result %+v", res)
	}

	if gotForm.Get("code") != "auth-code" {
		t.Errorf("code = %q", gotForm.Get("code"))
	}
	if gotForm.Get("code_verifier") != "verifier-abc" {
		t.Errorf("code_verifier = %q", gotForm.Get("code_verifier"))
	}
	// oauth2 may put client_secret in the form or in HTTP Basic (AuthStyleInHeader).
	secretOK := gotForm.Get("client_secret") == "secret-xyz" ||
		strings.Contains(gotAuth, "Basic ")
	if !secretOK {
		t.Errorf("client_secret missing: form=%q auth=%q", gotForm.Get("client_secret"), gotAuth)
	}
	if gotForm.Get("redirect_uri") != redirect {
		t.Errorf("redirect_uri = %q", gotForm.Get("redirect_uri"))
	}
	if gotForm.Has("state") {
		t.Error("exchange must not send state")
	}

	if len(accounts.saved) != 1 {
		t.Fatalf("saved %d connections", len(accounts.saved))
	}
	c := accounts.saved[0]
	aadAccess := vault.AAD("oauth_tokens", "tenant-1", "google", "sub-99", "access_token")
	plain, err := vlt.Decrypt(c.AccessEnc, c.AccessNonce, aadAccess)
	if err != nil || string(plain) != "access-plain" {
		t.Fatalf("decrypt access: %q err=%v", plain, err)
	}
	// Wrong column AAD must fail.
	if _, err := vlt.Decrypt(c.AccessEnc, c.AccessNonce, vault.AAD("oauth_tokens", "tenant-1", "google", "sub-99", "refresh_token")); !errors.Is(err, vault.ErrDecrypt) {
		t.Fatalf("wrong AAD err = %v", err)
	}
	aadRefresh := vault.AAD("oauth_tokens", "tenant-1", "google", "sub-99", "refresh_token")
	plain, err = vlt.Decrypt(c.RefreshEnc, c.RefreshNonce, aadRefresh)
	if err != nil || string(plain) != "refresh-plain" {
		t.Fatalf("decrypt refresh: %q err=%v", plain, err)
	}
}

func TestFinishAccessDeniedConsumesState(t *testing.T) {
	called := false
	_, spec := startFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(500)
	})
	states := &fakeStates{consume: map[string]store.ConsumedState{
		"google:st1": {TenantID: "tenant-1", CodeVerifier: "v"},
	}}
	svc, _ := newFinishService(t, states, &fakeAccounts{}, spec)

	_, err := svc.Finish(context.Background(), ProviderGoogle, "st1", "", "access_denied")
	if !errors.Is(err, ErrConsentDenied) {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("must not call Google after consent denied")
	}
	if _, err := states.Consume(context.Background(), "st1", "google"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("state must already be consumed")
	}
}

func TestFinishInvalidGrant(t *testing.T) {
	_, spec := startFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	states := &fakeStates{consume: map[string]store.ConsumedState{
		"google:st1": {TenantID: "t", CodeVerifier: "v"},
	}}
	svc, _ := newFinishService(t, states, &fakeAccounts{}, spec)

	_, err := svc.Finish(context.Background(), ProviderGoogle, "st1", "code", "")
	if !errors.Is(err, ErrExchangeRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestFinishGoogle500(t *testing.T) {
	_, spec := startFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	states := &fakeStates{consume: map[string]store.ConsumedState{
		"google:st1": {TenantID: "t", CodeVerifier: "v"},
	}}
	svc, _ := newFinishService(t, states, &fakeAccounts{}, spec)

	_, err := svc.Finish(context.Background(), ProviderGoogle, "st1", "code", "")
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestFinishInsufficientScope(t *testing.T) {
	_, spec := startFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		idTok := makeIDToken(t, "client-123", "https://accounts.google.com", "sub-1", time.Now().Add(time.Hour))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "a",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"scope":        GoogleOpenID, // calendar unchecked
			"id_token":     idTok,
		})
	})
	accounts := &fakeAccounts{}
	states := &fakeStates{consume: map[string]store.ConsumedState{
		"google:st1": {TenantID: "t", CodeVerifier: "v"},
	}}
	svc, _ := newFinishService(t, states, accounts, spec)

	_, err := svc.Finish(context.Background(), ProviderGoogle, "st1", "code", "")
	if !errors.Is(err, ErrInsufficientScope) {
		t.Fatalf("err = %v", err)
	}
	if len(accounts.saved) != 0 {
		t.Fatal("must not save when scope is insufficient")
	}
}

func TestFinishBadIDToken(t *testing.T) {
	cases := []struct {
		name string
		tok  func(t *testing.T) string
	}{
		{"bad aud", func(t *testing.T) string {
			return makeIDToken(t, "other-client", "https://accounts.google.com", "sub", time.Now().Add(time.Hour))
		}},
		{"bad iss", func(t *testing.T) string {
			return makeIDToken(t, "client-123", "https://evil.example", "sub", time.Now().Add(time.Hour))
		}},
		{"expired", func(t *testing.T) string {
			return makeIDToken(t, "client-123", "https://accounts.google.com", "sub", time.Now().Add(-time.Hour))
		}},
		{"empty sub", func(t *testing.T) string {
			return makeIDToken(t, "client-123", "https://accounts.google.com", "", time.Now().Add(time.Hour))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, spec := startFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "a",
					"token_type":   "Bearer",
					"expires_in":   3600,
					"scope":        GoogleOpenID + " " + GoogleCalendarReadonly,
					"id_token":     tc.tok(t),
				})
			})
			states := &fakeStates{consume: map[string]store.ConsumedState{
				"google:st1": {TenantID: "t", CodeVerifier: "v"},
			}}
			svc, _ := newFinishService(t, states, &fakeAccounts{}, spec)
			_, err := svc.Finish(context.Background(), ProviderGoogle, "st1", "code", "")
			if !errors.Is(err, ErrInvalidIDToken) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestFinishWithoutRefreshToken(t *testing.T) {
	_, spec := startFakeGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		idTok := makeIDToken(t, "client-123", "https://accounts.google.com", "sub-2", time.Now().Add(time.Hour))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "only-access",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"scope":        GoogleOpenID + " " + GoogleCalendarReadonly,
			"id_token":     idTok,
		})
	})
	accounts := &fakeAccounts{}
	states := &fakeStates{consume: map[string]store.ConsumedState{
		"google:st1": {TenantID: "tenant-1", CodeVerifier: "v"},
	}}
	svc, _ := newFinishService(t, states, accounts, spec)

	if _, err := svc.Finish(context.Background(), ProviderGoogle, "st1", "code", ""); err != nil {
		t.Fatal(err)
	}
	c := accounts.saved[0]
	if c.RefreshEnc != nil || c.RefreshNonce != nil {
		t.Fatalf("expected nil refresh, got enc=%v nonce=%v", c.RefreshEnc != nil, c.RefreshNonce != nil)
	}
	if !strings.Contains(strings.Join(c.Scopes, " "), GoogleCalendarReadonly) {
		t.Fatalf("scopes = %v", c.Scopes)
	}
}
