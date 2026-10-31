# ADR 0009: Rate limits live in Redis and fail open

## Status
Accepted (2026-10-30)

## Context
Any tenant with a valid API key could send unlimited requests. One client stuck in a retry loop could saturate Relay and spend the provider quota of every tenant, because provider calls share one OAuth app. ADR 0003 leaves Redis for locks and rate limiting; ADR 0006 keeps it out of token safety. A limit must hold across replicas, and a limiter must not become a new way for the API to go down.

## Decision
- Each tenant has a budget on authenticated `/v1` requests: `RATE_LIMIT_PER_MINUTE` and `RATE_LIMIT_BURST`, or the nullable column `tenants.rate_limit_per_minute`. The burst is capped to a lower override.
- The budget is GCRA through `redis_rate`: one Lua script per request. The check and the spend are atomic, it reads Redis's clock, and its keys expire.
- The middleware runs after `RequireAPIKey` because it needs the tenant. The override travels in the cached `store.Tenant`, so it adds no query.
- Past the budget, Relay answers `429 rate_limited` with `Retry-After` and the `RateLimit` and `RateLimit-Policy` fields of draft-ietf-httpapi-ratelimit-headers-11. On a 429, `t` equals the wait, because the draft forbids a `Retry-After` earlier than the end of the window.
- The OAuth callback and requests without a valid key count per IP (`RATE_LIMIT_IP_*`), using the socket address and a /64 for IPv6. A rejected key answers its usual `401` until that budget is spent. A valid key never spends the IP budget, so a tenant with a high override behind one address is not capped by it.
- `/healthz` and `/readyz` are never limited. The provider webhooks keep the per-IP limit in `notify`, so bad keys cannot spend the budget notifications need.
- When Redis fails, a `Failover` answers from an in-memory limiter (`golang.org/x/time/rate`) and leaves Redis alone for 5 seconds. Each Redis step is bounded to 100ms and nothing retries, so a failing Redis costs one request a few milliseconds. Each cooldown logs one warning with the count of local answers. `REDIS_URL` is optional: without it the local limiter is the only one, and startup warns.

## Consequences
- **Good:** One tenant cannot starve another. Clients can back off on their own. A Redis outage does not stop the API.
- **Bad:** During an outage, or without `REDIS_URL`, the real limit is N replicas times the configured one. Budgets reset when the outage ends.
- **Bad:** The socket address is the proxy's behind a reverse proxy, so all callers would share one IP budget. `X-Forwarded-For` is ignored because a client can forge it. A deployment behind a proxy must add a trusted way to read the client address first.
- **Bad:** A request with a well-formed, unknown key still costs a tenant lookup before it counts against the IP. The limit stops the requests, not that one query.
- **Bad:** This protects Relay, not provider quotas. An outbound limiter per account and provider is separate work. Providers' own throttling still reaches clients as `provider_rate_limited`.
- **Neutral:** There is no metrics system yet, so the fallback count lives in the log line.
