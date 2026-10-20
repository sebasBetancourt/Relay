package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/vault"
)

type tokenFakeAccounts struct {
	mu      sync.Mutex
	acct    store.StoredAccount
	updates []store.TokenUpdate
	revoked int
	getErr  error
}

func (f *tokenFakeAccounts) SaveConnection(context.Context, store.Connection) (string, error) {
	return "", errors.New("unused")
}

func (f *tokenFakeAccounts) AccountForTenant(context.Context, string, string) (store.StoredAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return store.StoredAccount{}, f.getErr
	}
	return f.acct, nil
}

func (f *tokenFakeAccounts) UpdateTokens(_ context.Context, _ string, u store.TokenUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, u)
	f.acct.AccessEnc = u.AccessEnc
	f.acct.AccessNonce = u.AccessNonce
	if u.RefreshEnc != nil {
		f.acct.RefreshEnc = u.RefreshEnc
		f.acct.RefreshNonce = u.RefreshNonce
	}
	f.acct.ExpiresAt = u.ExpiresAt
	return nil
}

func (f *tokenFakeAccounts) MarkRevoked(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked++
	f.acct.Status = "revoked"
	f.acct.HasTokens = false
	f.acct.AccessEnc = nil
	f.acct.RefreshEnc = nil
	return nil
}

func testVault(t *testing.T) *vault.Vault {
	t.Helper()
	key := make([]byte, vault.KeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	v, err := vault.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func encryptToken(t *testing.T, v *vault.Vault, tenant, provider, sub, col, plain string) (enc, nonce []byte) {
	t.Helper()
	enc, nonce, err := v.Encrypt([]byte(plain), tokenAAD(tenant, provider, sub, col))
	if err != nil {
		t.Fatal(err)
	}
	return enc, nonce
}

func seedActiveAccount(t *testing.T, v *vault.Vault, expires time.Time) store.StoredAccount {
	t.Helper()
	const tenant, provider, sub = "tenant-1", "google", "sub-1"
	aEnc, aN := encryptToken(t, v, tenant, provider, sub, "access_token", "access-live")
	rEnc, rN := encryptToken(t, v, tenant, provider, sub, "refresh_token", "refresh-live")
	return store.StoredAccount{
		ID: "acct-1", TenantID: tenant, Provider: provider, ProviderAccountID: sub,
		Status: "active", AccessEnc: aEnc, AccessNonce: aN,
		RefreshEnc: rEnc, RefreshNonce: rN, ExpiresAt: expires, HasTokens: true,
	}
}

func tokenService(t *testing.T, accounts AccountStore, v *vault.Vault, tokenURL string, refreshCalls *atomic.Int32) *Service {
	t.Helper()
	spec := ProviderSpec{
		OAuth: &oauth2.Config{
			ClientID: "client-123", ClientSecret: "secret",
			Endpoint: oauth2.Endpoint{TokenURL: tokenURL, AuthURL: tokenURL + "/auth"},
		},
	}
	return NewService(Deps{
		Accounts:   accounts,
		Vault:      v,
		Providers:  map[Provider]ProviderSpec{ProviderGoogle: spec},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
}

func TestAccessTokenFreshNoRefresh(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(time.Hour))
	fa := &tokenFakeAccounts{acct: acct}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)

	svc := tokenService(t, fa, v, srv.URL, &calls)
	tok, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "access-live" || tok.Provider != ProviderGoogle {
		t.Fatalf("tok = %+v", tok)
	}
	if calls.Load() != 0 {
		t.Fatal("must not call Google for fresh token")
	}
}

func TestAccessTokenExpiredRefreshes(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(-time.Minute))
	fa := &tokenFakeAccounts{acct: acct}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-new",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	}))
	t.Cleanup(srv.Close)

	svc := tokenService(t, fa, v, srv.URL, nil)
	tok, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "access-new" {
		t.Fatalf("value = %q", tok.Value)
	}
	if len(fa.updates) != 1 {
		t.Fatalf("updates = %d", len(fa.updates))
	}
	plain, err := v.Decrypt(fa.updates[0].AccessEnc, fa.updates[0].AccessNonce,
		tokenAAD("tenant-1", "google", "sub-1", "access_token"))
	if err != nil || string(plain) != "access-new" {
		t.Fatalf("decrypt: %q err=%v", plain, err)
	}
	_, err = v.Decrypt(fa.updates[0].AccessEnc, fa.updates[0].AccessNonce,
		tokenAAD("tenant-1", "google", "sub-1", "refresh_token"))
	if err == nil {
		t.Fatal("wrong-column AAD must fail")
	}
}

func TestAccessTokenRotatedRefreshSaved(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(-time.Minute))
	fa := &tokenFakeAccounts{acct: acct}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-rotated",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(srv.Close)

	svc := tokenService(t, fa, v, srv.URL, nil)
	if _, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1"); err != nil {
		t.Fatal(err)
	}
	u := fa.updates[0]
	if u.RefreshEnc == nil {
		t.Fatal("rotated refresh must be saved")
	}
	plain, err := v.Decrypt(u.RefreshEnc, u.RefreshNonce, tokenAAD("tenant-1", "google", "sub-1", "refresh_token"))
	if err != nil || string(plain) != "refresh-rotated" {
		t.Fatalf("refresh = %q err=%v", plain, err)
	}
}

func TestAccessTokenInvalidGrantRevokes(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(-time.Minute))
	fa := &tokenFakeAccounts{acct: acct}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	t.Cleanup(srv.Close)

	svc := tokenService(t, fa, v, srv.URL, nil)
	_, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1")
	if !errors.Is(err, ErrAccountRevoked) {
		t.Fatalf("err = %v", err)
	}
	if fa.revoked != 1 {
		t.Fatalf("revoked = %d", fa.revoked)
	}
}

func TestAccessTokenNoRefreshRevokes(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(-time.Minute))
	acct.RefreshEnc = nil
	acct.RefreshNonce = nil
	fa := &tokenFakeAccounts{acct: acct}
	svc := tokenService(t, fa, v, "http://127.0.0.1:1", nil)
	_, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1")
	if !errors.Is(err, ErrAccountRevoked) || fa.revoked != 1 {
		t.Fatalf("err=%v revoked=%d", err, fa.revoked)
	}
}

func TestAccessTokenProvider5xxNoRevoke(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(-time.Minute))
	fa := &tokenFakeAccounts{acct: acct}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	svc := tokenService(t, fa, v, srv.URL, nil)
	_, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1")
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if fa.revoked != 0 {
		t.Fatal("must not revoke on provider 5xx")
	}
}

func TestAccessTokenRevokedStatus(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(time.Hour))
	acct.Status = "revoked"
	fa := &tokenFakeAccounts{acct: acct}
	svc := tokenService(t, fa, v, "http://127.0.0.1:1", nil)
	_, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1")
	if !errors.Is(err, ErrAccountRevoked) {
		t.Fatalf("err = %v", err)
	}
}

func TestAccessTokenBadAAD(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(time.Hour))
	// Tamper ciphertext so decrypt fails.
	acct.AccessEnc[0] ^= 0xff
	fa := &tokenFakeAccounts{acct: acct}
	svc := tokenService(t, fa, v, "http://127.0.0.1:1", nil)
	_, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1")
	if err == nil || errors.Is(err, ErrAccountRevoked) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want internal decrypt error, got %v", err)
	}
}

func TestAccessTokenSingleflight(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(-time.Minute))
	fa := &tokenFakeAccounts{acct: acct}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-sf",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	}))
	t.Cleanup(srv.Close)

	svc := tokenService(t, fa, v, srv.URL, &calls)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := svc.AccessToken(context.Background(), "tenant-1", "acct-1")
			if err != nil {
				errs <- err
				return
			}
			if tok.Value != "access-sf" {
				errs <- errors.New("bad token")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
}
