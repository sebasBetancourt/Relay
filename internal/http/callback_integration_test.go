package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/connect"
	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/vault"
)

func TestCallbackEndToEnd(t *testing.T) {
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
		keyBytes[i] = byte(i + 3)
	}
	vlt, err := vault.New(keyBytes)
	if err != nil {
		t.Fatal(err)
	}

	idTok := makeGoogleIDToken(t, "client-123", "sub-e2e-"+apikey.Generate(apikey.EnvTest).KeyPrefix)
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "e2e-access",
			"refresh_token": "e2e-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         connect.GoogleOpenID + " " + connect.GoogleCalendarReadonly,
			"id_token":      idTok,
		})
	}))
	t.Cleanup(google.Close)

	base := connect.GoogleSpec("client-123", "secret-xyz", "http://localhost:8080/v1/oauth/google/callback")
	spec := connect.ProviderSpec{
		OAuth: &oauth2.Config{
			ClientID:     "client-123",
			ClientSecret: "secret-xyz",
			RedirectURL:  "http://localhost:8080/v1/oauth/google/callback",
			Endpoint:     oauth2.Endpoint{AuthURL: google.URL + "/auth", TokenURL: google.URL + "/token"},
			Scopes:       []string{connect.GoogleOpenID, connect.GoogleCalendarReadonly},
		},
		RequiredScope: base.RequiredScope,
		Identify:      base.Identify,
	}

	tenants := store.NewTenants(pool)
	key := apikey.Generate(apikey.EnvTest)
	tenant, err := tenants.Create(ctx, "Callback E2E", key.Hash, key.KeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID)
	})

	svc := connect.NewService(connect.Deps{
		States:     store.NewOAuthStates(pool),
		Accounts:   store.NewAccounts(pool),
		Vault:      vlt,
		Providers:  map[connect.Provider]connect.ProviderSpec{connect.ProviderGoogle: spec},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	router := NewRouter(Deps{
		Log:      discardLogger(),
		DB:       pool,
		Tenants:  tenants,
		Env:      apikey.EnvTest,
		Connect:  svc,
		Callback: svc,
	})

	// 1) Start connect
	req := httptest.NewRequest(http.MethodGet, "/v1/connect/google", nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("connect status = %d body %q", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")

	// 2) Callback
	cbURL := "/v1/oauth/google/callback?state=" + url.QueryEscape(state) + "&code=e2e-code"
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cbURL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("callback status = %d body %q", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	accountID := body["account_id"]
	if accountID == "" || body["status"] != "connected" {
		t.Fatalf("body %#v", body)
	}

	var (
		providerAccountID string
		accessEnc         []byte
		accessNonce       []byte
	)
	err = pool.QueryRow(ctx, `
		SELECT ca.provider_account_id, ot.access_token_enc, ot.access_nonce
		FROM connected_accounts ca
		JOIN oauth_tokens ot ON ot.account_id = ca.id
		WHERE ca.id = $1`, accountID,
	).Scan(&providerAccountID, &accessEnc, &accessNonce)
	if err != nil {
		t.Fatalf("read tokens: %v", err)
	}
	if string(accessEnc) == "e2e-access" {
		t.Fatal("plaintext access token stored in DB")
	}
	aad := vault.AAD("oauth_tokens", tenant.ID, "google", providerAccountID, "access_token")
	plain, err := vlt.Decrypt(accessEnc, accessNonce, aad)
	if err != nil || string(plain) != "e2e-access" {
		t.Fatalf("decrypt = %q err=%v", plain, err)
	}

	// 3) Replay callback → invalid_state
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cbURL, nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d", rec.Code)
	}
	var errBody errorBody
	_ = json.NewDecoder(rec.Body).Decode(&errBody)
	if errBody.Error != "invalid_state" {
		t.Fatalf("replay error = %q", errBody.Error)
	}
}

func makeGoogleIDToken(t *testing.T, aud, sub string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]any{
		"iss": "https://accounts.google.com",
		"aud": aud,
		"sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
