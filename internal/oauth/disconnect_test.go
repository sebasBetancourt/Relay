package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/store"
)

// disconnectFake returns prev from Disconnect, as the store would.
type disconnectFake struct {
	fakeAccounts
	prev store.StoredAccount
	err  error
}

func (f *disconnectFake) Disconnect(context.Context, string, string) (store.StoredAccount, error) {
	return f.prev, f.err
}

// revokeServer counts revocations and records the last token form value.
func revokeServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var calls atomic.Int32
	var token atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		token.Store(r.PostForm.Get("token"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &token
}

func disconnectService(t *testing.T, accounts AccountStore, provider Provider, revoke func(context.Context, *http.Client, string) error) *Service {
	t.Helper()
	return NewService(Deps{
		Accounts:   accounts,
		Vault:      testVault(t),
		Providers:  map[Provider]ProviderSpec{provider: {OAuth: &oauth2.Config{}, Revoke: revoke}},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
}

func TestDisconnectRevokesRefreshTokenAtGoogle(t *testing.T) {
	srv, calls, token := revokeServer(t, http.StatusOK, `{}`)
	fake := &disconnectFake{prev: seedActiveAccount(t, testVault(t), time.Now().Add(time.Hour))}
	svc := disconnectService(t, fake, ProviderGoogle, GoogleRevoke(srv.URL))

	if err := svc.Disconnect(context.Background(), "tenant-1", "acct-1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || token.Load() != "refresh-live" {
		t.Fatalf("revoke calls = %d token = %v, want 1 call with the refresh token", calls.Load(), token.Load())
	}
}

func TestDisconnectSucceedsWhenRevokeFails(t *testing.T) {
	srv, calls, _ := revokeServer(t, http.StatusServiceUnavailable, `{}`)
	fake := &disconnectFake{prev: seedActiveAccount(t, testVault(t), time.Now().Add(time.Hour))}
	svc := disconnectService(t, fake, ProviderGoogle, GoogleRevoke(srv.URL))

	// The local disconnect already committed: a provider outage is logged,
	// not returned.
	if err := svc.Disconnect(context.Background(), "tenant-1", "acct-1"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("revoke calls = %d", calls.Load())
	}
}

func TestDisconnectWithoutRevokeOrTokens(t *testing.T) {
	v := testVault(t)
	revoked := 0
	revoke := func(context.Context, *http.Client, string) error { revoked++; return nil }

	tests := []struct {
		name   string
		prev   store.StoredAccount
		revoke func(context.Context, *http.Client, string) error
	}{
		{"provider without revocation (Microsoft)", seedActiveAccount(t, v, time.Now()), nil},
		{"already disconnected", store.StoredAccount{ID: "acct-1", Provider: "google", Status: "disconnected"}, revoke},
		{"revoked account has no tokens", store.StoredAccount{ID: "acct-1", Provider: "google", Status: "revoked"}, revoke},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := disconnectService(t, &disconnectFake{prev: tt.prev}, ProviderGoogle, tt.revoke)
			if err := svc.Disconnect(context.Background(), "tenant-1", "acct-1"); err != nil {
				t.Fatal(err)
			}
		})
	}
	if revoked != 0 {
		t.Fatalf("revoke called %d times, want 0", revoked)
	}
}

func TestDisconnectPropagatesNotFound(t *testing.T) {
	svc := disconnectService(t, &disconnectFake{err: store.ErrNotFound}, ProviderGoogle, nil)
	if err := svc.Disconnect(context.Background(), "tenant-1", "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGoogleRevoke(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
		wantOK  bool
	}{
		{"revoked", 200, `{}`, nil, true},
		{"already invalid counts as revoked", 400, `{"error":"invalid_token"}`, nil, true},
		{"google down", 503, `{}`, ErrProviderUnavailable, false},
		{"other 4xx", 401, `{"error":"unauthorized_client"}`, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotForm url.Values
			var gotType string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				gotForm, gotType = r.PostForm, r.Header.Get("Content-Type")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			err := GoogleRevoke(srv.URL)(context.Background(), srv.Client(), "tok-123")
			switch {
			case tt.wantOK && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case !tt.wantOK && err == nil:
				t.Fatal("want an error")
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if gotForm.Get("token") != "tok-123" || gotType != "application/x-www-form-urlencoded" {
				t.Fatalf("request form = %v type = %q", gotForm, gotType)
			}
		})
	}
}

func TestAccessTokenDisconnectedAccount(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(time.Hour))
	acct.Status = "disconnected"
	svc := tokenService(t, &tokenFakeAccounts{acct: acct}, v, "http://127.0.0.1:1", nil)

	_, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1")
	if !errors.Is(err, ErrAccountDisconnected) || errors.Is(err, ErrAccountRevoked) {
		t.Fatalf("err = %v, want ErrAccountDisconnected (not revoked)", err)
	}
}
