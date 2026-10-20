package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/connect"
	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/vault"
)

func TestListEventsEndToEndRefreshAndRevoke(t *testing.T) {
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
		keyBytes[i] = byte(i + 11)
	}
	vlt, err := vault.New(keyBytes)
	if err != nil {
		t.Fatal(err)
	}

	var tokenHits, calHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	})
	mux.HandleFunc("/calendars/primary/events", func(w http.ResponseWriter, r *http.Request) {
		calHits++
		if r.Header.Get("Authorization") != "Bearer fresh-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"timeZone": "UTC",
			"items": []map[string]any{
				{
					"id": "ev-1", "summary": "Sync",
					"start": map[string]string{"dateTime": "2026-10-20T15:00:00Z"},
					"end":   map[string]string{"dateTime": "2026-10-20T16:00:00Z"},
				},
			},
		})
	})
	google := httptest.NewServer(mux)
	t.Cleanup(google.Close)

	tenants := store.NewTenants(pool)
	accounts := store.NewAccounts(pool)
	key := apikey.Generate(apikey.EnvTest)
	tenant, err := tenants.Create(ctx, "Cal E2E", key.Hash, key.KeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	otherKey := apikey.Generate(apikey.EnvTest)
	other, err := tenants.Create(ctx, "Cal Other", otherKey.Hash, otherKey.KeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = ANY($1)", []string{tenant.ID, other.ID})
	})

	sub := "sub-cal-e2e-" + key.KeyPrefix
	aadAccess := vault.AAD("oauth_tokens", tenant.ID, "google", sub, "access_token")
	aadRefresh := vault.AAD("oauth_tokens", tenant.ID, "google", sub, "refresh_token")
	accessEnc, accessNonce, err := vlt.Encrypt([]byte("stale-access"), aadAccess)
	if err != nil {
		t.Fatal(err)
	}
	refreshEnc, refreshNonce, err := vlt.Encrypt([]byte("refresh-ok"), aadRefresh)
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := accounts.SaveConnection(ctx, store.Connection{
		TenantID: tenant.ID, Provider: "google", ProviderAccountID: sub,
		AccessEnc: accessEnc, AccessNonce: accessNonce,
		RefreshEnc: refreshEnc, RefreshNonce: refreshNonce,
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
		Scopes:    []string{connect.GoogleOpenID, connect.GoogleCalendarReadonly},
	})
	if err != nil {
		t.Fatal(err)
	}

	connectSvc := connect.NewService(connect.Deps{
		States:   store.NewOAuthStates(pool),
		Accounts: accounts,
		Vault:    vlt,
		Providers: map[connect.Provider]connect.ProviderSpec{
			connect.ProviderGoogle: {
				OAuth: &oauth2.Config{
					ClientID: "c", ClientSecret: "s",
					Endpoint: oauth2.Endpoint{TokenURL: google.URL + "/token", AuthURL: google.URL + "/auth"},
				},
			},
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Log:        discardLogger(),
	})
	calSvc := calendar.NewService(connectSvc, map[connect.Provider]calendar.Source{
		connect.ProviderGoogle: &calendar.GoogleSource{BaseURL: google.URL, HTTP: google.Client()},
	})
	router := NewRouter(Deps{
		Log: discardLogger(), DB: pool, Tenants: tenants, Env: apikey.EnvTest,
		Connect: connectSvc, Callback: connectSvc, Calendar: calSvc,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/calendars/"+accountID+"/events?max_results=5", nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if tokenHits != 1 || calHits != 1 {
		t.Fatalf("tokenHits=%d calHits=%d", tokenHits, calHits)
	}
	var body eventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Events) != 1 || body.Events[0].Title != "Sync" {
		t.Fatalf("body = %+v", body)
	}
	stored, err := accounts.AccountForTenant(ctx, accountID, tenant.ID)
	if err != nil || stored.ExpiresAt.Before(time.Now()) {
		t.Fatalf("tokens not refreshed: %+v err=%v", stored, err)
	}

	// Other tenant → 404
	req = httptest.NewRequest(http.MethodGet, "/v1/calendars/"+accountID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+otherKey.Plaintext)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("other tenant status %d", rec.Code)
	}
}

func TestListEventsInvalidGrantRevokes(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	keyBytes := make([]byte, vault.KeySize)
	for i := range keyBytes {
		keyBytes[i] = byte(i + 13)
	}
	vlt, err := vault.New(keyBytes)
	if err != nil {
		t.Fatal(err)
	}

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	t.Cleanup(tokenSrv.Close)

	tenants := store.NewTenants(pool)
	accounts := store.NewAccounts(pool)
	key := apikey.Generate(apikey.EnvTest)
	tenant, err := tenants.Create(ctx, "Cal Revoke", key.Hash, key.KeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID)
	})

	sub := "sub-revoke-" + key.KeyPrefix
	aEnc, aN, _ := vlt.Encrypt([]byte("a"), vault.AAD("oauth_tokens", tenant.ID, "google", sub, "access_token"))
	rEnc, rN, _ := vlt.Encrypt([]byte("r"), vault.AAD("oauth_tokens", tenant.ID, "google", sub, "refresh_token"))
	accountID, err := accounts.SaveConnection(ctx, store.Connection{
		TenantID: tenant.ID, Provider: "google", ProviderAccountID: sub,
		AccessEnc: aEnc, AccessNonce: aN, RefreshEnc: rEnc, RefreshNonce: rN,
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
		Scopes:    []string{"openid"},
	})
	if err != nil {
		t.Fatal(err)
	}

	connectSvc := connect.NewService(connect.Deps{
		States: store.NewOAuthStates(pool), Accounts: accounts, Vault: vlt,
		Providers: map[connect.Provider]connect.ProviderSpec{
			connect.ProviderGoogle: {
				OAuth: &oauth2.Config{
					ClientID: "c", ClientSecret: "s",
					Endpoint: oauth2.Endpoint{TokenURL: tokenSrv.URL, AuthURL: tokenSrv.URL},
				},
			},
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Log:        discardLogger(),
	})
	calSvc := calendar.NewService(connectSvc, map[connect.Provider]calendar.Source{
		connect.ProviderGoogle: &calendar.GoogleSource{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: time.Second}},
	})
	router := NewRouter(Deps{
		Log: discardLogger(), DB: pool, Tenants: tenants, Env: apikey.EnvTest, Calendar: calSvc,
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/calendars/"+accountID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var body errorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error != "account_revoked" {
		t.Fatalf("body = %+v", body)
	}
	stored, err := accounts.AccountForTenant(ctx, accountID, tenant.ID)
	if err != nil || stored.Status != "revoked" || stored.HasTokens {
		t.Fatalf("stored = %+v err=%v", stored, err)
	}
}
