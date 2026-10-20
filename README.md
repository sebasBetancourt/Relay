# Relay

[![CI](https://github.com/sebasBetancourt/Relay/actions/workflows/ci.yml/badge.svg)](https://github.com/sebasBetancourt/Relay/actions/workflows/ci.yml)

Unified API for Google and Microsoft calendars and mail: connect an account once, then read, write and receive real-time changes through the same API regardless of the provider.

## Stack

**HTTP:** Go standard library [`net/http`](https://pkg.go.dev/net/http) (with Go 1.22+ method-aware routing on `ServeMux`).

We use `net/http` instead of a framework (Gin, Echo, Chi, etc.) because:

- Zero extra dependency — the router and server stay in the stdlib.
- Enough for this API: path/method routing, middleware as plain `http.Handler` wrappers, and clear control over status codes and bodies.
- Keeps the binary and Docker image lean, consistent with shipping a single Go artifact.
- Avoids learning and locking into framework conventions we do not need yet.

## Getting started

Requirements: Docker, [golang-migrate](https://github.com/golang-migrate/migrate) CLI, and (optionally) Go 1.25+ to run the API outside Docker.

```bash
cp .env.example .env                 # then fill in the Google vars and ENCRYPTION_KEY
docker compose up -d --build
make migrate-up
curl http://localhost:8080/healthz   # ok
curl http://localhost:8080/readyz    # ready
```

Notes:

- Postgres is published on host port **5435** (`5435:5432`); `DATABASE_URL` in `.env` and `make migrate-up` use that.
- Inside Compose, the API talks to Postgres as `postgres:5432`.
- If host port **8080** is already in use, stop the other process or change the API port mapping in `docker-compose.yml`.
- The API fails fast on startup and lists every missing or invalid variable at once. `RELAY_ENV` (`live` or `test`) is required.
- Google credentials: follow [docs/google-cloud-setup.md](docs/google-cloud-setup.md).
- Expired `oauth_states` rows are purged by a janitor inside the API (on startup and every 10 minutes).

## Running the tests

```bash
# Unit tests (no DB): always safe
go test ./internal/apikey ./internal/vault ./internal/janitor ./internal/testutil/...

# Full suite including integration (Postgres on :5435)
TEST_DATABASE_URL='postgres://relay:relay@localhost:5435/relay?sslmode=disable' \
  go test -race -count=1 ./...
```

Integration tests use a **stateful fake Google** (`internal/testutil/fakegoogle`): it checks PKCE, one-time codes, refresh/access validity and calendar Bearer tokens. The end-to-end flow suite is `go test -run Flow ./test/e2e` (black-box: real router, services and Postgres, fake Google).

## Create a tenant and test your API key

A **tenant** is a Relay customer. Each tenant calls the API with an **API key** (`rly_test_…` or `rly_live_…`, matching `RELAY_ENV`). Relay stores only the SHA-256 hash of the key, so it is shown **once**, when it is created.

1. Create a tenant (needs `DATABASE_URL` and `RELAY_ENV`):

   ```bash
   make tenant NAME="Acme"
   ```

   Details go to stderr and the key alone goes to stdout, so you can save it straight to a file:

   ```bash
   go run ./cmd/relayctl tenant create --name "Acme" > acme.key
   ```

2. Call the API with the key in the `Authorization` header:

   ```bash
   curl -i localhost:8080/v1/me                                         # 401
   curl -i -H "Authorization: Bearer rly_test_fake" localhost:8080/v1/me  # 401
   curl -i -H "Authorization: Bearer $(cat acme.key)" localhost:8080/v1/me
   # 200 {"id":"…","name":"Acme","key_prefix":"rly_test_xxxxxxxx"}
   ```

How authentication works:

- `/healthz` and `/readyz` are public. Everything under `/v1/` requires `Authorization: Bearer <key>`.
- Malformed keys, bad checksums and keys from the other environment are rejected without touching the database.
- Every client error gets the same `401 {"error":"unauthorized"}`; the reason is only logged, never the key itself. A database failure returns `500`, not `401`.
- Revoked keys stop working immediately on the instance that revoked them, and within 30 seconds (cache TTL) on the others.

Key format and secret scanning: [docs/secret-scanning.md](docs/secret-scanning.md).

## Connect a Google account

`GET /v1/connect/{provider}` starts the OAuth flow (authorization code + PKCE) for the calling tenant. Only `google` is enabled for now.

```bash
# Default: 302 to Google's consent screen (open the Location in a browser)
curl -i -H "Authorization: Bearer $(cat acme.key)" localhost:8080/v1/connect/google

# For backends: get the URL as JSON and hand it to your end user's browser
curl -H "Authorization: Bearer $(cat acme.key)" -H "Accept: application/json" \
  localhost:8080/v1/connect/google
# {"authorization_url":"https://accounts.google.com/o/oauth2/auth?..."}
```

| Case | Status |
| --- | --- |
| Consent URL created | `302` (or `200` JSON with `Accept: application/json`) |
| Missing or invalid API key | `401 {"error":"unauthorized"}` |
| Unknown or not yet enabled provider | `400 {"error":"unsupported_provider"}` |
| Database failure | `500 {"error":"internal error"}` |

Relay stores a one-time `state` and the PKCE `code_verifier` in `oauth_states` for 10 minutes; only the `code_challenge` (SHA-256 of the verifier) travels in the URL. Scopes are `openid` + `calendar.readonly`, with `access_type=offline` and `prompt=consent` so Google returns a refresh token and an `id_token`.

### Callback (after Google consent)

`GET /v1/oauth/{provider}/callback` is **public** (the browser has no API key). Security is the one-time `state`. On success:

```json
{"status":"connected","provider":"google","account_id":"…"}
```

Tokens are encrypted (AES-256-GCM) and never returned in the body. Reloading the same callback URL returns `400 {"error":"invalid_state"}`.

| Case | Status | `error` |
| --- | --- | --- |
| Connected | `200` | — |
| Missing `state` / `code` | `400` | `invalid_request` |
| Unknown provider | `400` | `unsupported_provider` |
| Bad / reused / expired state | `400` | `invalid_state` |
| User denied consent | `400` | `access_denied` |
| Google rejected the code | `400` | `exchange_failed` |
| Calendar scope unchecked | `400` | `insufficient_scope` |
| Bad `id_token` | `502` | `provider_error` |
| Google down / timeout | `502` | `provider_unavailable` |
| Account owned by another tenant | `409` | `account_already_connected` |
| DB / vault failure | `500` | `internal error` |

## Read events

`GET /v1/calendars/{id}/events` lists events from the connected account’s **primary** calendar. `{id}` is the `account_id` returned by the OAuth callback (`connected_accounts.id`). Requires an API key.

```bash
# account_id from the callback JSON, or: SELECT id FROM connected_accounts;
curl -s -H "Authorization: Bearer $(cat demo.key)" \
  "localhost:8080/v1/calendars/<account_id>/events?max_results=5" | jq
```

| Query | Default | Notes |
| --- | --- | --- |
| `time_min` | now (RFC3339) | |
| `time_max` | `time_min + 30d` | Must be after `time_min`; max range 366 days |
| `max_results` | 50 | 1–250 |
| `page_token` | — | Opaque; pass through from a previous response |

Example response:

```json
{
  "events": [
    {
      "id": "…",
      "title": "Standup",
      "start": "2026-10-20T09:00:00-05:00",
      "end": "2026-10-20T09:30:00-05:00",
      "timezone": "America/Bogota",
      "all_day": false,
      "location": "Zoom",
      "organizer": {"email": "a@ex.com", "name": "Ada"},
      "provider": "google"
    }
  ],
  "next_page_token": "…"
}
```

Expired access tokens are refreshed silently (60s skew, singleflight per account). If Google rejects the refresh, the account is marked `revoked` and the client gets **409** (reconnect via `/v1/connect/google`).

| Case | Status | `error` |
| --- | --- | --- |
| OK | 200 | — |
| Invalid query | 400 | `invalid_query` |
| Missing / wrong-tenant account | 404 | `not_found` |
| Account revoked / refresh invalid | 409 | `account_revoked` |
| Google rate limit | 429 | `provider_rate_limited` (+ `Retry-After`) |
| Google down / timeout | 502 | `provider_unavailable` |
| Unexpected provider response | 502 | `provider_error` |
| DB / decrypt failure | 500 | `internal error` |
