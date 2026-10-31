package middleware

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/sebasBetancourt/relay/internal/ratelimit"
	"github.com/sebasBetancourt/relay/internal/store"
)

// ipv6PrefixBits groups IPv6 clients: one customer usually holds a whole /64,
// so limiting single addresses would be no limit at all.
const ipv6PrefixBits = 64

// RateLimiter holds the per-tenant and per-IP budgets. A nil *RateLimiter
// limits nothing, so tests and callers can leave it out.
type RateLimiter struct {
	log     *slog.Logger
	limiter ratelimit.Limiter
	tenant  ratelimit.Limit
	ip      ratelimit.Limit
}

func NewRateLimiter(log *slog.Logger, limiter ratelimit.Limiter, tenant, ip ratelimit.Limit) *RateLimiter {
	return &RateLimiter{log: log, limiter: limiter, tenant: tenant, ip: ip}
}

// PerTenant limits authenticated requests. It must run after RequireAPIKey,
// which puts the tenant in the context.
func (l *RateLimiter) PerTenant(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := TenantFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		limit := l.tenantLimit(t)
		if l.enforce(w, r, "tenant", "rl:tenant:"+t.ID, limit) {
			next.ServeHTTP(w, r)
		}
	})
}

// PerIP limits public routes, which have no tenant to count against.
func (l *RateLimiter) PerIP(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.enforce(w, r, "ip", ipKey(r), l.ip) {
			next.ServeHTTP(w, r)
		}
	})
}

// rejectedKey counts a request without a valid key against its IP and reports
// whether it was answered with 429. Headers are left out of the normal 401:
// a caller without a valid key gets the same body and headers as always.
func (l *RateLimiter) rejectedKey(w http.ResponseWriter, r *http.Request) bool {
	if l == nil {
		return false
	}
	res, ok := l.take(r, ipKey(r), l.ip)
	if !ok || res.Allowed {
		return false
	}
	writeRateLimited(w, "ip", l.ip, res)
	return true
}

// tenantLimit applies the override. The burst is capped to it: a tenant
// limited to 10 per minute must not get the global burst of 100 at once.
func (l *RateLimiter) tenantLimit(t store.Tenant) ratelimit.Limit {
	limit := l.tenant
	if t.RateLimitPerMinute != nil {
		limit.PerMinute = *t.RateLimitPerMinute
		limit.Burst = min(limit.Burst, limit.PerMinute)
	}
	return limit
}

// enforce reports whether the request may continue, having answered it if not.
func (l *RateLimiter) enforce(w http.ResponseWriter, r *http.Request, policy, key string, limit ratelimit.Limit) bool {
	res, ok := l.take(r, key, limit)
	if !ok {
		return true
	}
	if !res.Allowed {
		writeRateLimited(w, policy, limit, res)
		return false
	}
	setRateLimitHeaders(w, policy, limit, res.Remaining, res.ResetAfter)
	return true
}

// take fails open: a limiter error lets the request through, because the
// API must not depend on the limiter being healthy.
func (l *RateLimiter) take(r *http.Request, key string, limit ratelimit.Limit) (ratelimit.Result, bool) {
	res, err := l.limiter.Allow(r.Context(), key, limit)
	if err != nil {
		if r.Context().Err() == nil {
			l.log.Warn("rate limit check failed",
				"err", err,
				"request_id", RequestIDFromContext(r.Context()),
			)
		}
		return ratelimit.Result{}, false
	}
	return res, true
}

// writeRateLimited sets t to the wait: the draft forbids a Retry-After earlier
// than the end of the window the RateLimit header announces.
func writeRateLimited(w http.ResponseWriter, policy string, limit ratelimit.Limit, res ratelimit.Result) {
	setRateLimitHeaders(w, policy, limit, 0, res.RetryAfter)
	w.Header().Set("Retry-After", strconv.Itoa(max(ceilSeconds(res.RetryAfter), 1)))
	writeError(w, http.StatusTooManyRequests, "rate_limited")
}

// setRateLimitHeaders follows draft-ietf-httpapi-ratelimit-headers-11.
func setRateLimitHeaders(w http.ResponseWriter, policy string, limit ratelimit.Limit, remaining int, reset time.Duration) {
	w.Header().Set("RateLimit-Policy", fmt.Sprintf("%q;q=%d;w=60", policy, limit.PerMinute))
	w.Header().Set("RateLimit", fmt.Sprintf("%q;r=%d;t=%d", policy, remaining, ceilSeconds(reset)))
}

func ceilSeconds(d time.Duration) int {
	return int(math.Ceil(d.Seconds()))
}

func ipKey(r *http.Request) string {
	return "rl:ip:" + clientIP(r)
}

// clientIP uses the socket address. X-Forwarded-For is ignored on purpose:
// without a trusted proxy in front, any client can write it and pick its bucket.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		if prefix, err := addr.Prefix(ipv6PrefixBits); err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}
