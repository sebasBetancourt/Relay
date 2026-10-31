package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/ratelimit"
	"github.com/sebasBetancourt/relay/internal/store"
)

type fakeLimiter struct {
	res   ratelimit.Result
	err   error
	key   string
	limit ratelimit.Limit
	calls int
}

func (f *fakeLimiter) Allow(_ context.Context, key string, limit ratelimit.Limit) (ratelimit.Result, error) {
	f.calls++
	f.key, f.limit = key, limit
	return f.res, f.err
}

var (
	tenantLimit = ratelimit.Limit{PerMinute: 600, Burst: 100}
	ipLimit     = ratelimit.Limit{PerMinute: 1200, Burst: 200}
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

func servePerTenant(l ratelimit.Limiter, tenant store.Tenant) *httptest.ResponseRecorder {
	rl := NewRateLimiter(discardLogger(), l, tenantLimit, ipLimit)
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req = req.WithContext(context.WithValue(req.Context(), tenantKey, tenant))
	rec := httptest.NewRecorder()
	rl.PerTenant(okHandler()).ServeHTTP(rec, req)
	return rec
}

func TestPerTenantAllowedSetsHeaders(t *testing.T) {
	l := &fakeLimiter{res: ratelimit.Result{Allowed: true, Remaining: 41, ResetAfter: 1500 * time.Millisecond}}
	rec := servePerTenant(l, store.Tenant{ID: "t-1"})

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	if l.key != "rl:tenant:t-1" {
		t.Fatalf("key = %q", l.key)
	}
	if got := rec.Header().Get("RateLimit-Policy"); got != `"tenant";q=600;w=60` {
		t.Fatalf("policy = %q", got)
	}
	if got := rec.Header().Get("RateLimit"); got != `"tenant";r=41;t=2` {
		t.Fatalf("ratelimit = %q", got)
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("Retry-After belongs on a 429 only")
	}
}

func TestPerTenantRejected(t *testing.T) {
	l := &fakeLimiter{res: ratelimit.Result{RetryAfter: 2100 * time.Millisecond}}
	rec := servePerTenant(l, store.Tenant{ID: "t-1"})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != "rate_limited" {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, want the wait rounded up", got)
	}
	// The draft forbids a Retry-After earlier than the window's end.
	if got := rec.Header().Get("RateLimit"); got != `"tenant";r=0;t=3` {
		t.Fatalf("ratelimit = %q", got)
	}
}

func TestRetryAfterIsNeverZero(t *testing.T) {
	rec := servePerTenant(&fakeLimiter{}, store.Tenant{ID: "t-1"})
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want at least 1", got)
	}
}

func TestPerTenantFailsOpen(t *testing.T) {
	rec := servePerTenant(&fakeLimiter{err: errors.New("limiter down")}, store.Tenant{ID: "t-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, a limiter failure must not fail the request", rec.Code)
	}
}

func TestPerTenantOverride(t *testing.T) {
	ten, high := 10, 6000
	tests := []struct {
		name     string
		override *int
		want     ratelimit.Limit
	}{
		{"no override uses global", nil, tenantLimit},
		{"lower override caps the burst too", &ten, ratelimit.Limit{PerMinute: 10, Burst: 10}},
		{"higher override keeps the burst", &high, ratelimit.Limit{PerMinute: 6000, Burst: 100}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &fakeLimiter{res: ratelimit.Result{Allowed: true}}
			servePerTenant(l, store.Tenant{ID: "t-1", RateLimitPerMinute: tt.override})
			if l.limit != tt.want {
				t.Fatalf("limit = %+v, want %+v", l.limit, tt.want)
			}
		})
	}
}

func TestNilRateLimiterLimitsNothing(t *testing.T) {
	var rl *RateLimiter
	for _, h := range []http.Handler{rl.PerTenant(okHandler()), rl.PerIP(okHandler())} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d", rec.Code)
		}
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct{ remote, want string }{
		{"203.0.113.9:1234", "203.0.113.9"},
		{"[::ffff:203.0.113.9]:1234", "203.0.113.9"},
		{"[2001:db8:1:2:3:4:5:6]:1234", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:ffff:4:5:6]:1234", "2001:db8:1:2::/64"},
		{"[fe80::1%eth0]:1234", "fe80::/64"},
		{"not-an-address", "not-an-address"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tt.remote
		if got := clientIP(req); got != tt.want {
			t.Errorf("clientIP(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}

func TestClientIPIgnoresForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Fatalf("clientIP = %q: a client must not pick its own bucket", got)
	}
}

// A flood of bad keys never reaches a tenant budget: it spends the IP budget,
// and a client holding a valid key from the same IP keeps working.
func TestRejectedKeysCountAgainstIP(t *testing.T) {
	good := apikey.Generate(apikey.EnvTest)
	lookup := &fakeLookup{tenants: map[string]store.Tenant{good.Hash: {ID: "t-1"}}}
	rl := NewRateLimiter(discardLogger(), ratelimit.NewLocal(),
		ratelimit.Limit{PerMinute: 600, Burst: 100}, ratelimit.Limit{PerMinute: 1, Burst: 2})
	h := RequireAPIKey(discardLogger(), lookup, apikey.EnvTest, rl)(rl.PerTenant(okHandler()))

	do := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		req.RemoteAddr = "203.0.113.9:1234"
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for range 2 {
		rec := do("Bearer nope")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code = %d, want 401 inside the budget", rec.Code)
		}
		if rec.Header().Get("RateLimit") != "" {
			t.Fatal("a plain 401 must look the same as without a limiter")
		}
	}
	rec := do("Bearer nope")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("code = %d, want 429 past the IP budget", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("a 429 is not an auth challenge")
	}
	if rec := do("Bearer " + good.Plaintext); rec.Code != http.StatusOK {
		t.Fatalf("valid key from the same IP: code = %d", rec.Code)
	}
	if !strings.Contains(do("Bearer "+good.Plaintext).Header().Get("RateLimit"), `"tenant"`) {
		t.Fatal("a valid key must be counted against its tenant")
	}
}
