package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	gosync "sync"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/httpapi/middleware"
	"github.com/sebasBetancourt/relay/internal/ratelimit"
)

func redisLimiter(t *testing.T) *ratelimit.Redis {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set, skipping integration test")
	}
	r, err := ratelimit.OpenRedis(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// limitedReplica is another API instance with its own limiter client.
func (f *flow) limitedReplica(l ratelimit.Limiter, tenant, ip ratelimit.Limit) http.Handler {
	f.t.Helper()
	f.rateLimit = middleware.NewRateLimiter(discardLogger(), l, tenant, ip)
	defer func() { f.rateLimit = nil }()
	return f.buildRouter(newPool(f.t, f.dbURL))
}

func (f *flow) otherTenantKey() map[string]string {
	f.t.Helper()
	key := apikey.Generate(apikey.EnvTest)
	tenant, err := f.tenants.Create(f.ctx, "Other "+sanitizeName(f.t.Name()), key.Hash, key.KeyPrefix)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM tenants WHERE id = $1", tenant.ID)
	})
	return map[string]string{"Authorization": "Bearer " + key.Plaintext}
}

// Requests split across two replicas spend one budget. The rate is low enough
// that nothing refills during the test, so exactly the burst may pass.
func TestRateLimitSharedAcrossReplicas(t *testing.T) {
	f := newFlow(t)
	limit := ratelimit.Limit{PerMinute: 1, Burst: 10}
	ip := ratelimit.Limit{PerMinute: 1000, Burst: 1000}
	routers := []http.Handler{
		f.limitedReplica(redisLimiter(t), limit, ip),
		f.limitedReplica(redisLimiter(t), limit, ip),
	}

	const total = 40
	var wg gosync.WaitGroup
	codes := make(chan int, total)
	for i := range total {
		wg.Add(1)
		go func(r http.Handler) {
			defer wg.Done()
			codes <- doOn(r, http.MethodGet, "/v1/me", f.authHdr()).Code
		}(routers[i%2])
	}
	wg.Wait()
	close(codes)

	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[http.StatusOK] != limit.Burst || counts[http.StatusTooManyRequests] != total-limit.Burst {
		t.Fatalf("codes = %v, want %d OK and the rest 429", counts, limit.Burst)
	}
}

func TestRateLimitNoisyTenantDoesNotAffectAnother(t *testing.T) {
	f := newFlow(t)
	router := f.limitedReplica(redisLimiter(t), ratelimit.Limit{PerMinute: 1, Burst: 2},
		ratelimit.Limit{PerMinute: 1000, Burst: 1000})
	quiet := f.otherTenantKey()

	var last int
	for range 5 {
		last = doOn(router, http.MethodGet, "/v1/me", f.authHdr()).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("noisy tenant got %d, want 429", last)
	}
	if rec := doOn(router, http.MethodGet, "/v1/me", quiet); rec.Code != http.StatusOK {
		t.Fatalf("quiet tenant got %d, want 200", rec.Code)
	}
}

func TestRateLimitRejectedResponse(t *testing.T) {
	f := newFlow(t)
	router := f.limitedReplica(redisLimiter(t), ratelimit.Limit{PerMinute: 1, Burst: 1},
		ratelimit.Limit{PerMinute: 1000, Burst: 1000})

	if rec := doOn(router, http.MethodGet, "/v1/me", f.authHdr()); rec.Code != http.StatusOK {
		t.Fatalf("first request %d", rec.Code)
	}
	rec := doOn(router, http.MethodGet, "/v1/me", f.authHdr())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "rate_limited" {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q", ra)
	}
}

// The override travels from the column through the tenant lookup to the limiter.
func TestRateLimitTenantOverrideWins(t *testing.T) {
	f := newFlow(t)
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE tenants SET rate_limit_per_minute = 1 WHERE id = $1`, f.tenant.ID); err != nil {
		t.Fatal(err)
	}
	router := f.limitedReplica(redisLimiter(t), ratelimit.Limit{PerMinute: 1000, Burst: 1000},
		ratelimit.Limit{PerMinute: 1000, Burst: 1000})

	if rec := doOn(router, http.MethodGet, "/v1/me", f.authHdr()); rec.Code != http.StatusOK {
		t.Fatalf("first request %d", rec.Code)
	}
	if rec := doOn(router, http.MethodGet, "/v1/me", f.authHdr()); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request %d, want 429 from the override", rec.Code)
	}
}

func TestRateLimitNeverTouchesProbes(t *testing.T) {
	f := newFlow(t)
	spent := ratelimit.Limit{PerMinute: 1, Burst: 1}
	router := f.limitedReplica(ratelimit.NewLocal(), spent, spent)
	for range 5 {
		for _, path := range []string{"/healthz", "/readyz"} {
			if rec := doOn(router, http.MethodGet, path, nil); rec.Code != http.StatusOK {
				t.Fatalf("%s got %d", path, rec.Code)
			}
		}
	}
}

func TestRateLimitPublicRouteUsesIPBudget(t *testing.T) {
	f := newFlow(t)
	router := f.limitedReplica(ratelimit.NewLocal(), ratelimit.Limit{PerMinute: 1000, Burst: 1000},
		ratelimit.Limit{PerMinute: 1, Burst: 2})
	var last int
	for range 3 {
		last = doOn(router, http.MethodGet, "/v1/oauth/google/callback?state=x&code=y", nil).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429 past the IP budget", last)
	}
}

// Nothing listens on the Redis port: the API must keep answering and the
// fallback must keep limiting.
func TestRateLimitRedisDownFailsOpen(t *testing.T) {
	f := newFlow(t)
	down, err := ratelimit.OpenRedis("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = down.Close() })
	limiter := ratelimit.NewFailover(slog.New(slog.DiscardHandler), down, ratelimit.NewLocal())
	router := f.limitedReplica(limiter, ratelimit.Limit{PerMinute: 1, Burst: 2},
		ratelimit.Limit{PerMinute: 1000, Burst: 1000})

	for range 2 {
		if rec := doOn(router, http.MethodGet, "/v1/me", f.authHdr()); rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 with Redis down", rec.Code)
		}
	}
	if rec := doOn(router, http.MethodGet, "/v1/me", f.authHdr()); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want the local limit to hold", rec.Code)
	}
}
