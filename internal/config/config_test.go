package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
)

var validKey = base64.StdEncoding.EncodeToString(make([]byte, 32))

func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://relay:relay@localhost:5432/relay")
	t.Setenv("GOOGLE_CLIENT_ID", "client-id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "client-secret")
	t.Setenv("GOOGLE_REDIRECT_URL", "http://localhost:8080/v1/oauth/google/callback")
	t.Setenv("ENCRYPTION_KEY", validKey)
	t.Setenv("RELAY_ENV", "test")
	// Isolate from a developer shell that exports real Microsoft values.
	for _, k := range []string{
		"MICROSOFT_CLIENT_ID", "MICROSOFT_CLIENT_SECRET", "MICROSOFT_REDIRECT_URL", "MICROSOFT_TENANT_ID", "MICROSOFT_EXPIRATION_DATE",
		"REFRESH_AHEAD", "REFRESH_CONCURRENCY",
		"WEBHOOK_BASE_URL", "NATS_URL",
		"REDIS_URL", "RATE_LIMIT_PER_MINUTE", "RATE_LIMIT_BURST", "RATE_LIMIT_IP_PER_MINUTE", "RATE_LIMIT_IP_BURST",
	} {
		t.Setenv(k, "")
	}
}

func setMicrosoftEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MICROSOFT_CLIENT_ID", "ms-client-id")
	t.Setenv("MICROSOFT_CLIENT_SECRET", "ms-client-secret")
	t.Setenv("MICROSOFT_REDIRECT_URL", "http://localhost:8080/v1/oauth/microsoft/callback")
}

func clearGoogleEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "GOOGLE_REDIRECT_URL"} {
		t.Setenv(k, "")
	}
}

// fixedNow keeps expiry tests independent of the real date.
var fixedNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestLoadValid(t *testing.T) {
	setValidEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.EncryptionKey) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(cfg.EncryptionKey))
	}
	if cfg.Env != apikey.EnvTest {
		t.Fatalf("expected env test, got %q", cfg.Env)
	}
	if cfg.Port != "8080" {
		t.Fatalf("expected default port 8080, got %q", cfg.Port)
	}
	if cfg.RefreshAhead != 10*time.Minute || cfg.RefreshConcurrency != 8 {
		t.Fatalf("refresh defaults = %s / %d", cfg.RefreshAhead, cfg.RefreshConcurrency)
	}
	if cfg.WebhookBaseURL != "" || cfg.NATSURL != "" {
		t.Fatalf("push should default off, base %q nats %q", cfg.WebhookBaseURL, cfg.NATSURL)
	}
}

func TestLoadWebhookLoopback(t *testing.T) {
	setValidEnv(t)
	t.Setenv("WEBHOOK_BASE_URL", "http://127.0.0.1:8080/")
	t.Setenv("NATS_URL", "nats://127.0.0.1:4222")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookBaseURL != "http://127.0.0.1:8080" || cfg.NATSURL != "nats://127.0.0.1:4222" {
		t.Fatalf("base %q nats %q", cfg.WebhookBaseURL, cfg.NATSURL)
	}
}

func TestLoadRateLimitDefaults(t *testing.T) {
	setValidEnv(t)
	cfg, err := load(fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisURL != "" || cfg.RateLimitPerMinute != 600 || cfg.RateLimitBurst != 100 ||
		cfg.RateLimitIPPerMinute != 1200 || cfg.RateLimitIPBurst != 200 {
		t.Fatalf("defaults: %+v", cfg)
	}
}

func TestRedisURLErrorHidesPassword(t *testing.T) {
	setValidEnv(t)
	t.Setenv("REDIS_URL", "http://user:s3cret@localhost:6379")
	_, err := load(fixedNow)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{"missing database url", "DATABASE_URL", "", "DATABASE_URL is required"},
		{"rate limit zero", "RATE_LIMIT_PER_MINUTE", "0", "RATE_LIMIT_PER_MINUTE"},
		{"rate limit negative", "RATE_LIMIT_PER_MINUTE", "-5", "RATE_LIMIT_PER_MINUTE"},
		{"rate limit burst zero", "RATE_LIMIT_BURST", "0", "RATE_LIMIT_BURST"},
		{"ip rate limit zero", "RATE_LIMIT_IP_PER_MINUTE", "0", "RATE_LIMIT_IP_PER_MINUTE"},
		{"ip burst not a number", "RATE_LIMIT_IP_BURST", "many", "RATE_LIMIT_IP_BURST"},
		{"redis wrong scheme", "REDIS_URL", "http://localhost:6379", "REDIS_URL"},
		{"redis without host", "REDIS_URL", "redis://", "REDIS_URL"},
		{"google id without secret", "GOOGLE_CLIENT_SECRET", "", "GOOGLE_CLIENT_SECRET is required when GOOGLE_CLIENT_ID is set"},
		{"google secret without id", "GOOGLE_CLIENT_ID", "", "GOOGLE_CLIENT_ID is required when GOOGLE_CLIENT_SECRET is set"},
		{"missing redirect url", "GOOGLE_REDIRECT_URL", "", "GOOGLE_REDIRECT_URL: is required"},
		{"relative redirect url", "GOOGLE_REDIRECT_URL", "/v1/oauth/google/callback", "absolute http(s) URL"},
		{"wrong scheme redirect url", "GOOGLE_REDIRECT_URL", "ftp://localhost/cb", "absolute http(s) URL"},
		{"missing encryption key", "ENCRYPTION_KEY", "", "ENCRYPTION_KEY: is required"},
		{"encryption key not base64", "ENCRYPTION_KEY", "not base64!!", "must be valid base64"},
		{"missing relay env", "RELAY_ENV", "", "RELAY_ENV: is required"},
		{"unknown relay env", "RELAY_ENV", "prod", `must be "live" or "test", got "prod"`},
		{"uppercase relay env", "RELAY_ENV", "LIVE", `got "LIVE"`},
		{"encryption key too short", "ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)), "got 16"},
		{"refresh ahead not a duration", "REFRESH_AHEAD", "soon", "REFRESH_AHEAD"},
		{"refresh ahead zero", "REFRESH_AHEAD", "0s", "REFRESH_AHEAD"},
		{"refresh concurrency zero", "REFRESH_CONCURRENCY", "0", "REFRESH_CONCURRENCY"},
		{"public http webhook", "WEBHOOK_BASE_URL", "http://example.com", "must be https"},
		{"webhook without nats", "WEBHOOK_BASE_URL", "https://relay.example", "NATS_URL is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := Load()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	setValidEnv(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ENCRYPTION_KEY", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, want := range []string{"DATABASE_URL", "ENCRYPTION_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to mention %s, got %q", want, err)
		}
	}
}

func TestLoadNeverLeaksEncryptionKey(t *testing.T) {
	setValidEnv(t)
	secret := base64.StdEncoding.EncodeToString([]byte("sixteen-byte-key"))
	t.Setenv("ENCRYPTION_KEY", secret)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error message leaks the encryption key")
	}
}

func TestLoadLiveEnv(t *testing.T) {
	setValidEnv(t)
	t.Setenv("RELAY_ENV", "live")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Env != apikey.EnvLive {
		t.Fatalf("expected env live, got %q", cfg.Env)
	}
}

func TestLoadProviderCombinations(t *testing.T) {
	tests := []struct {
		name          string
		google, msft  bool
		wantProviders []string
	}{
		{"google only", true, false, []string{"google"}},
		{"microsoft only", false, true, []string{"microsoft"}},
		{"both", true, true, []string{"google", "microsoft"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setValidEnv(t)
			if !tt.google {
				clearGoogleEnv(t)
			}
			if tt.msft {
				setMicrosoftEnv(t)
			}

			cfg, err := load(fixedNow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := strings.Join(cfg.Providers(), ","); got != strings.Join(tt.wantProviders, ",") {
				t.Fatalf("providers = %q, want %q", got, tt.wantProviders)
			}
			if (cfg.Google != nil) != tt.google || (cfg.Microsoft != nil) != tt.msft {
				t.Fatalf("google=%v microsoft=%v", cfg.Google != nil, cfg.Microsoft != nil)
			}
		})
	}
}

func TestLoadNoProvider(t *testing.T) {
	setValidEnv(t)
	clearGoogleEnv(t)
	// A redirect URL alone (as in .env.example) does not enable a provider.
	t.Setenv("GOOGLE_REDIRECT_URL", "http://localhost:8080/v1/oauth/google/callback")
	t.Setenv("MICROSOFT_REDIRECT_URL", "http://localhost:8080/v1/oauth/microsoft/callback")

	_, err := load(fixedNow)
	if err == nil || !strings.Contains(err.Error(), "no provider configured") {
		t.Fatalf("err = %v, want no provider configured", err)
	}
}

func TestLoadMicrosoftDefaults(t *testing.T) {
	setValidEnv(t)
	setMicrosoftEnv(t)

	cfg, err := load(fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Microsoft
	if m.Tenant != "common" {
		t.Fatalf("tenant = %q, want common", m.Tenant)
	}
	if m.ClientID != "ms-client-id" || m.RedirectURL != "http://localhost:8080/v1/oauth/microsoft/callback" {
		t.Fatalf("client = %+v", m.OAuthClient)
	}
	if !m.SecretExpiresAt.IsZero() {
		t.Fatal("expiry must be zero when unset")
	}
	if w := cfg.Warnings(fixedNow); len(w) != 0 {
		t.Fatalf("unexpected warnings: %v", w)
	}
}

func TestLoadMicrosoftInvalid(t *testing.T) {
	tests := []struct {
		name, key, value, wantErr string
	}{
		{"id without secret", "MICROSOFT_CLIENT_SECRET", "", "MICROSOFT_CLIENT_SECRET is required when MICROSOFT_CLIENT_ID is set"},
		{"missing redirect", "MICROSOFT_REDIRECT_URL", "", "MICROSOFT_REDIRECT_URL: is required"},
		{"relative redirect", "MICROSOFT_REDIRECT_URL", "/v1/oauth/microsoft/callback", "absolute http(s) URL"},
		{"tenant with path", "MICROSOFT_TENANT_ID", "common/../evil", "MICROSOFT_TENANT_ID"},
		{"tenant with spaces", "MICROSOFT_TENANT_ID", "my tenant", "MICROSOFT_TENANT_ID"},
		{"bad expiry format", "MICROSOFT_EXPIRATION_DATE", "18/04/2027", "must be YYYY-MM-DD"},
		{"expired secret", "MICROSOFT_EXPIRATION_DATE", "2026-10-01", "client secret expired on 2026-10-01"},
		{"expires today", "MICROSOFT_EXPIRATION_DATE", "2026-10-20", "client secret expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setValidEnv(t)
			setMicrosoftEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := load(fixedNow)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "ms-client-secret") {
				t.Fatal("error leaks the client secret")
			}
		})
	}
}

func TestLoadMicrosoftTenants(t *testing.T) {
	for _, tenant := range []string{"common", "organizations", "consumers", "Common",
		"72f988bf-86f1-41af-91ab-2d7cd011db47", "contoso.onmicrosoft.com"} {
		t.Run(tenant, func(t *testing.T) {
			setValidEnv(t)
			setMicrosoftEnv(t)
			t.Setenv("MICROSOFT_TENANT_ID", tenant)

			cfg, err := load(fixedNow)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Microsoft.Tenant != tenant {
				t.Fatalf("tenant = %q", cfg.Microsoft.Tenant)
			}
		})
	}
}

func TestWarningsSecretExpiry(t *testing.T) {
	tests := []struct {
		date     string
		wantWarn bool
	}{
		{"2027-04-18", false}, // ~6 months away
		{"2026-11-25", false}, // 36 days
		{"2026-11-10", true},  // 20 days
		{"2026-10-21", true},  // tomorrow
	}
	for _, tt := range tests {
		t.Run(tt.date, func(t *testing.T) {
			setValidEnv(t)
			setMicrosoftEnv(t)
			t.Setenv("MICROSOFT_EXPIRATION_DATE", tt.date)

			cfg, err := load(fixedNow)
			if err != nil {
				t.Fatal(err)
			}
			w := cfg.Warnings(fixedNow)
			if (len(w) > 0) != tt.wantWarn {
				t.Fatalf("warnings = %v, want warn=%v", w, tt.wantWarn)
			}
			if tt.wantWarn && !strings.Contains(w[0], tt.date) {
				t.Fatalf("warning should name the date: %q", w[0])
			}
		})
	}
}
