package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

// TestConnectWithRealDB runs router → middleware.RequireAPIKey → oauth.Service
// → store → Postgres and checks the state in the Location is really stored.
func TestConnectWithRealDB(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()

	pool, err := store.NewPool(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	tenants := store.NewTenants(pool)
	key := apikey.Generate(apikey.EnvTest)
	tenant, err := tenants.Create(ctx, "Connect Integration", key.Hash, key.KeyPrefix)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	// Cascade also removes the oauth_states row.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID)
	})

	svc := oauth.NewService(oauth.Deps{
		States: store.NewOAuthStates(pool),
		Providers: map[oauth.Provider]oauth.ProviderSpec{
			oauth.ProviderGoogle: oauth.GoogleSpec("client-123", "secret-xyz", "http://localhost:8080/v1/oauth/google/callback"),
			oauth.ProviderMicrosoft: oauth.MicrosoftSpec("ms-client", "ms-secret",
				"http://localhost:8080/v1/oauth/microsoft/callback", "common", oauth.MicrosoftGraphURL),
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	router := contractRouter(t, Deps{
		Log:      discardLogger(),
		DB:       pool,
		Tenants:  tenants,
		Env:      apikey.EnvTest,
		Connect:  svc,
		Callback: svc,
	})

	for _, provider := range []string{"google", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/connect/"+provider, nil)
			req.Header.Set("Authorization", "Bearer "+key.Plaintext)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want 302 (body %q)", rec.Code, rec.Body.String())
			}
			loc, err := url.Parse(rec.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			state := loc.Query().Get("state")

			var tenantID, gotProvider string
			err = pool.QueryRow(ctx,
				"SELECT tenant_id::text, provider::text FROM oauth_states WHERE state = $1 AND expires_at > now() AND used_at IS NULL",
				state,
			).Scan(&tenantID, &gotProvider)
			if err != nil {
				t.Fatalf("state from Location not found as a live row: %v", err)
			}
			if tenantID != tenant.ID || gotProvider != provider {
				t.Fatalf("row = %s/%s, want %s/%s", tenantID, gotProvider, tenant.ID, provider)
			}
		})
	}
}
