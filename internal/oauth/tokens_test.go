package oauth

import (
	"bytes"
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
	mu    sync.Mutex
	rowMu sync.Mutex // plays the Postgres row lock in RefreshLocked
	acct  store.StoredAccount
	// beforeLock runs right before the "lock" is taken: lets a test play
	// another replica that refreshes while this request waits.
	beforeLock func(f *tokenFakeAccounts)
	// commitErr makes RefreshLocked fail after fn succeeded (lost commit).
	commitErr error
	updates   []store.TokenUpdate
	revoked   int
	getErr    error
}

func (f *tokenFakeAccounts) SaveConnection(context.Context, store.Connection) (string, error) {
	return "", errors.New("unused")
}

func (f *tokenFakeAccounts) AccountForTenant(context.Context, string, string) (store.StoredAccount, error) {
	return f.AccountByID(context.Background(), "")
}

func (f *tokenFakeAccounts) AccountByID(context.Context, string) (store.StoredAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return store.StoredAccount{}, f.getErr
	}
	return f.acct, nil
}

// RefreshLocked mirrors the store: one caller at a time, fn sees the latest
// row, and its update is applied before the "lock" is released.
func (f *tokenFakeAccounts) RefreshLocked(_ context.Context, _ string, fn func(store.StoredAccount) (*store.TokenUpdate, error)) error {
	if f.beforeLock != nil {
		f.beforeLock(f)
	}
	f.rowMu.Lock()
	defer f.rowMu.Unlock()

	f.mu.Lock()
	cur := f.acct
	f.mu.Unlock()
	if cur.Status != "active" || !cur.HasTokens {
		return store.ErrNotFound
	}
	u, err := fn(cur)
	if err != nil {
		return err
	}
	if f.commitErr != nil {
		return f.commitErr // rolled back: the update is lost
	}
	if u != nil {
		f.apply(*u)
	}
	return nil
}

func (f *tokenFakeAccounts) apply(u store.TokenUpdate) {
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

func TestRefreshAccountDisconnectAndMissingRefresh(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(-time.Minute))
	acct.Status = "disconnected"
	fa := &tokenFakeAccounts{acct: acct}
	svc := tokenService(t, fa, v, "http://127.0.0.1:1", nil)
	err := svc.RefreshAccount(context.Background(), acct.ID)
	if !errors.Is(err, ErrAccountDisconnected) || fa.revoked != 0 {
		t.Fatalf("err=%v revoked=%d", err, fa.revoked)
	}

	acct = seedActiveAccount(t, v, time.Now().Add(-time.Minute))
	acct.RefreshEnc = nil
	acct.RefreshNonce = nil
	fa = &tokenFakeAccounts{acct: acct}
	svc = tokenService(t, fa, v, "http://127.0.0.1:1", nil)
	err = svc.RefreshAccount(context.Background(), acct.ID)
	if !errors.Is(err, ErrAccountRevoked) || fa.revoked != 1 {
		t.Fatalf("missing refresh: err=%v revoked=%d", err, fa.revoked)
	}
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

// tokenServer answers the token endpoint with body/status and counts calls.
func tokenServer(t *testing.T, status int, body string, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Microsoft returns interaction_required when MFA, Conditional Access or new
// consent needs the user: only a new connect fixes it.
func TestAccessTokenInteractionRequiredRevokes(t *testing.T) {
	v := testVault(t)
	fa := &tokenFakeAccounts{acct: seedActiveAccount(t, v, time.Now().Add(-time.Minute))}
	srv := tokenServer(t, http.StatusBadRequest,
		`{"error":"interaction_required","error_description":"AADSTS50076: multi-factor authentication required"}`, nil)

	_, err := tokenService(t, fa, v, srv.URL, nil).AccessToken(context.Background(), "tenant-1", "acct-1")
	if !errors.Is(err, ErrAccountRevoked) || fa.revoked != 1 {
		t.Fatalf("err=%v revoked=%d, want ErrAccountRevoked and 1 revoke", err, fa.revoked)
	}
}

// Another replica refreshes while this request waits for the row lock: this
// request must reuse that token, not refresh again (with Microsoft, a second
// refresh would rotate the token the other replica just stored).
func TestAccessTokenUsesTokenRefreshedByOtherReplica(t *testing.T) {
	v := testVault(t)
	fa := &tokenFakeAccounts{acct: seedActiveAccount(t, v, time.Now().Add(-time.Minute))}
	fa.beforeLock = func(f *tokenFakeAccounts) {
		enc, nonce := encryptToken(t, v, "tenant-1", "google", "sub-1", "access_token", "access-from-replica-b")
		f.mu.Lock()
		f.acct.AccessEnc, f.acct.AccessNonce = enc, nonce
		f.acct.ExpiresAt = time.Now().Add(time.Hour)
		f.mu.Unlock()
	}
	var calls atomic.Int32
	srv := tokenServer(t, http.StatusOK, `{"access_token":"should-not-be-used","expires_in":3600}`, &calls)

	tok, err := tokenService(t, fa, v, srv.URL, nil).AccessToken(context.Background(), "tenant-1", "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "access-from-replica-b" {
		t.Fatalf("value = %q, want the token the other replica stored", tok.Value)
	}
	if calls.Load() != 0 || len(fa.updates) != 0 {
		t.Fatalf("refresh calls = %d, updates = %d, want 0 and 0", calls.Load(), len(fa.updates))
	}
}

// The provider rotated the refresh token but the commit failed: return an
// error, do not revoke, and keep the previous refresh token so the next
// request can retry.
func TestAccessTokenLostCommitKeepsAccount(t *testing.T) {
	v := testVault(t)
	acct := seedActiveAccount(t, v, time.Now().Add(-time.Minute))
	fa := &tokenFakeAccounts{acct: acct, commitErr: errors.New("connection reset")}
	srv := tokenServer(t, http.StatusOK,
		`{"access_token":"access-new","refresh_token":"refresh-rotated","expires_in":3600}`, nil)

	_, err := tokenService(t, fa, v, srv.URL, nil).AccessToken(context.Background(), "tenant-1", "acct-1")
	if err == nil || errors.Is(err, ErrAccountRevoked) {
		t.Fatalf("err = %v, want a plain (500) error", err)
	}
	if fa.revoked != 0 || fa.acct.Status != "active" {
		t.Fatal("a lost commit must not revoke the account")
	}
	if !bytes.Equal(fa.acct.RefreshEnc, acct.RefreshEnc) {
		t.Fatal("the previous refresh token must stay in place")
	}
}

func (f *tokenFakeAccounts) Disconnect(context.Context, string, string) (store.StoredAccount, error) {
	return store.StoredAccount{}, store.ErrNotFound
}
