package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/store"
)

// rotatingProvider is a strict Microsoft-like token endpoint: every refresh
// returns a new refresh token and invalidates the one it received.
type rotatingProvider struct {
	mu      sync.Mutex
	valid   string // the only refresh token currently accepted
	n       int
	refresh int
}

func (p *rotatingProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.PostForm.Get("refresh_token") != p.valid {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS70008: refresh token already used"}`))
		return
	}
	time.Sleep(150 * time.Millisecond) // a slow provider widens the race window
	p.n++
	p.refresh++
	p.valid = fmt.Sprintf("refresh-%d", p.n)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  fmt.Sprintf("access-%d", p.n),
		"refresh_token": p.valid,
		"expires_in":    3600,
		"token_type":    "Bearer",
	})
}

// TestRefreshRotationAcrossReplicas runs two oauth.Services with separate
// pools (two API replicas, each with its own singleflight) against Postgres.
// Both see an expired token at the same time: exactly one may call the
// provider, both must get the same access token, and the rotated refresh
// token must be the one stored.
func TestRefreshRotationAcrossReplicas(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	newPool := func() *store.Accounts {
		pool, err := store.NewPool(ctx, dbURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		return store.NewAccounts(pool)
	}

	// Seed a tenant and a Microsoft account whose access token has expired.
	seedPool, err := store.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(seedPool.Close)
	key := apikey.Generate(apikey.EnvTest)
	tenant, err := store.NewTenants(seedPool).Create(ctx, "Rotation", key.Hash, key.KeyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = seedPool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID) })

	v := testVault(t)
	sub := "ms-rotation-" + tenant.ID[:8]
	aEnc, aN := encryptToken(t, v, tenant.ID, "microsoft", sub, "access_token", "access-0")
	rEnc, rN := encryptToken(t, v, tenant.ID, "microsoft", sub, "refresh_token", "refresh-0")
	accountID, err := store.NewAccounts(seedPool).SaveConnection(ctx, store.Connection{
		TenantID: tenant.ID, Provider: "microsoft", ProviderAccountID: sub,
		AccessEnc: aEnc, AccessNonce: aN, RefreshEnc: rEnc, RefreshNonce: rN,
		ExpiresAt: time.Now().Add(-time.Minute), Scopes: []string{MicrosoftCalendarsRead},
	})
	if err != nil {
		t.Fatal(err)
	}

	provider := &rotatingProvider{valid: "refresh-0"}
	srv := httptest.NewServer(provider)
	t.Cleanup(srv.Close)
	replica := func() *Service {
		return NewService(Deps{
			Accounts: newPool(),
			Vault:    v,
			Providers: map[Provider]ProviderSpec{ProviderMicrosoft: {
				OAuth: &oauth2.Config{
					ClientID: "ms-client", ClientSecret: "ms-secret",
					Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams},
				},
			}},
			HTTPClient: &http.Client{Timeout: 5 * time.Second},
		})
	}
	replicas := []*Service{replica(), replica()}

	// Both replicas, several requests each, all at once.
	var wg sync.WaitGroup
	results := make(chan string, 8)
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func(svc *Service) {
			defer wg.Done()
			tok, err := svc.AccessToken(ctx, tenant.ID, accountID)
			if err != nil {
				errs <- err
				return
			}
			results <- tok.Value
		}(replicas[i%2])
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("request failed: %v", err)
	}
	for got := range results {
		if got != "access-1" {
			t.Fatalf("got %q, want every request to share access-1", got)
		}
	}
	if provider.refresh != 1 {
		t.Fatalf("provider refreshed %d times, want exactly 1", provider.refresh)
	}

	// The stored refresh token is the rotated one: a forced refresh works
	// against the strict provider (it would fail with refresh-0).
	tok, err := replicas[1].RefreshAccessToken(ctx, tenant.ID, accountID)
	if err != nil {
		t.Fatalf("forced refresh with the stored token: %v", err)
	}
	if tok.Value != "access-2" || provider.refresh != 2 {
		t.Fatalf("forced refresh = %q (refreshes %d), want access-2 after 2", tok.Value, provider.refresh)
	}
}
