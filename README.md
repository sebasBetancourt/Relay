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
cp .env.example .env                 # then fill in ENCRYPTION_KEY and at least one provider
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
- Providers are optional; configure at least one:
  - Google: [docs/google-cloud-setup.md](docs/google-cloud-setup.md)
  - Microsoft (Outlook): [docs/microsoft-setup.md](docs/microsoft-setup.md)
- The startup log lists the enabled providers (`"providers":["google","microsoft"]`).
- Expired `oauth_states` rows are purged by a janitor inside the API (on startup and every 10 minutes).

## Build and run locally

```bash
make build                 # bin/relay and bin/relayctl (bin/ is gitignored)
./bin/relay                # reads .env; same as: go run ./cmd/api
```

| Make target | What it does |
| --- | --- |
| `make build` | Compile `bin/relay` (API) and `bin/relayctl` (admin CLI) |
| `make migrate-up` / `make migrate-down` | Apply all migrations / roll back the last one |
| `make tenant NAME="Acme"` | Create a tenant and print its API key once |

## Running the tests

```bash
# Unit tests only: integration tests skip themselves without TEST_DATABASE_URL
go test -race ./...

# Full suite: unit + integration + end-to-end (Postgres on :5435)
TEST_DATABASE_URL='postgres://relay:relay@localhost:5435/relay?sslmode=disable' \
  go test -race -count=1 ./...

# Only the end-to-end flow (connect → callback → events)
TEST_DATABASE_URL='postgres://relay:relay@localhost:5435/relay?sslmode=disable' \
  go test -race -count=1 ./test/e2e
```

| Kind | Where | Needs Postgres |
| --- | --- | --- |
| Unit | `*_test.go` next to the code they test | No |
| Integration | `*_integration_test.go` (store, httpapi) | Yes |
| End-to-end | `test/e2e`, black-box through HTTP using exported APIs only | Yes |

Integration and e2e tests run against **stateful fake providers**, so CI needs no internet and no real accounts:

- `internal/testutil/fakeoauth`: the shared OAuth core. It handles one-time codes bound to PKCE challenges, refresh and access validity, revocation, simulated failures and latency.
- `fakegoogle`: Google OAuth, the `id_token`, token revocation and the Calendar events API.
- `fakemicrosoft`: Entra ID and Graph `/me` and `calendarView`. It is stricter than Microsoft on purpose:
  - every refresh rotates the refresh token and kills the old one;
  - the client secret must travel in the body;
  - `@odata.nextLink` points at the real Graph host, so following it instead of keeping `$skiptoken` fails.

`test/e2e` runs every scenario (happy path, invalid state, expired-token refresh, revoked refresh) for both Google and Microsoft. It also covers Microsoft-only cases (refresh rotation, two API replicas refreshing at once, paging, `interaction_required`, no mailbox) and the accounts lifecycle.

## Project layout

Packages are organized by responsibility, not by layer ([ADR 0004](docs/adr/0004-project-layout.md)). Everything under `internal/` can only be imported by this module.

```
cmd/
  api/                      API entry point; the only place that reads config and wires packages
  relayctl/                 admin CLI (create tenants)
internal/
  config/                   env loading and validation (fail fast)
  httpapi/                  router and handlers: me, connect, callback, calendars
    middleware/             request id, access log, panic recovery, API key auth
  oauth/                    connect flow (Start/Finish) and token lifecycle (decrypt, refresh, revoke)
  calendar/                 events service + provider sources (Google)
  store/                    Postgres access, SQL only
  apikey/                   API key generation, checksum and hashing
  vault/                    AES-256-GCM encryption with AAD for tokens at rest
  janitor/                  background purge of expired oauth_states
  testutil/fakegoogle/      stateful fake Google for tests
test/e2e/                   end-to-end flow suite
migrations/                 SQL migrations (golang-migrate)
docs/                       design, ADRs, Google Cloud setup, secret scanning
bin/                        build output (gitignored)
```

Dependencies point one way: `httpapi → calendar → oauth → store`. `vault`, `apikey` and `store` import nothing internal. Interfaces are declared by the package that uses them (e.g. `middleware.TenantLookup`, `oauth.StateStore`), so each package can be tested with small fakes.

## Create a tenant and test your API key

A **tenant** is a Relay customer. Each tenant calls the API with an **API key** (`rly_test_…` or `rly_live_…`, matching `RELAY_ENV`). Relay stores only the SHA-256 hash of the key, so it is shown **once**, when it is created.

1. Create a tenant (needs `DATABASE_URL` and `RELAY_ENV`):

   ```bash
   make tenant NAME="Acme"
   ```

   Details go to stderr and the key alone goes to stdout, so you can save it straight to a file:

   ```bash
   make tenant NAME="Acme" > acme.key
   # or, after make build: ./bin/relayctl tenant create --name "Acme" > acme.key
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

## Connect an account (Google or Microsoft)

`GET /v1/connect/{provider}` starts the OAuth flow (authorization code + PKCE) for the calling tenant. `{provider}` is `google` or `microsoft` (Outlook / Microsoft 365), whichever is configured. The examples use `google`; Microsoft works the same way.

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
{"status":"connected","provider":"google","account_id":"…","email":"ana@gmail.com"}
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

`GET /v1/calendars/{id}/events` lists events from the connected account’s **primary** calendar, Google or Outlook, in the same shape. `{id}` is the `account_id` returned by the OAuth callback (`connected_accounts.id`). Requires an API key.

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
| `page_token` | — | Opaque; pass through from a previous response. It carries the first page's window and size, so other parameters are ignored with it |

Paging: keep following `next_page_token` until it is absent. A page may hold fewer events than `max_results` (providers cap page sizes, and cancelled Outlook occurrences are dropped), but never zero unless it is the last one.

Example response:

```json
{
  "events": [
    {
      "id": "…",
      "title": "Standup",
      "start": "2026-10-20T14:00:00Z",
      "end": "2026-10-20T14:30:00Z",
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

Normalization, for both providers:

- `start` / `end`: RFC3339 for timed events, `YYYY-MM-DD` for all-day events (`end` exclusive). Outlook times are returned in UTC (`Z`).
- `timezone`: the event's IANA zone. Outlook's Windows names (`"SA Pacific Standard Time"`) are mapped with CLDR (`America/Bogota`); unknown zones are left empty, never guessed.
- Recurring events are expanded into occurrences.

Expired access tokens are refreshed silently: 60s skew, singleflight per account, and a row lock in Postgres so replicas never race (Microsoft rotates the refresh token on every refresh). If the provider rejects the refresh, the account is marked `revoked` and the client gets **409** (reconnect via `/v1/connect/{provider}`).

| Case | Status | `error` |
| --- | --- | --- |
| OK | 200 | — |
| Invalid query | 400 | `invalid_query` |
| Missing / wrong-tenant account | 404 | `not_found` |
| Account revoked / refresh invalid / MFA required | 409 | `account_revoked` |
| Account disconnected by the tenant | 409 | `account_disconnected` |
| Microsoft account without an Outlook mailbox | 409 | `calendar_unavailable` |
| Provider rate limit | 429 | `provider_rate_limited` (+ `Retry-After`) |
| Provider down / timeout | 502 | `provider_unavailable` |
| Unexpected provider response | 502 | `provider_error` |
| DB / decrypt failure | 500 | `internal error` |

## Manage connected accounts

```bash
# List the tenant's accounts (no tokens, ever)
curl -s -H "Authorization: Bearer $(cat acme.key)" "localhost:8080/v1/accounts?limit=50" | jq
# {"accounts":[{"id":"…","provider":"microsoft","email":"ana@outlook.com","status":"active","connected_at":"…"}],"next_cursor":"…"}

# Disconnect one
curl -i -X DELETE -H "Authorization: Bearer $(cat acme.key)" localhost:8080/v1/accounts/<account_id>
# 204 No Content
```

- `GET /v1/accounts`: `limit` 1–100 (default 50). Pass `cursor=<next_cursor>` for the next page; the cursor is absent on the last page. `status` is `active`, `revoked` (the provider rejected the refresh token) or `disconnected`.
- `DELETE /v1/accounts/{id}`: deletes the tokens and marks the account `disconnected` (204). It is idempotent and the account can be connected again.
  - Google tokens are also revoked at Google (best effort; a Google outage never blocks the disconnect).
  - Microsoft has no per-app revocation endpoint: users remove the app at account.microsoft.com → Privacy → Apps and services.
- Another tenant's account, or one that does not exist, gives `404 not_found` on both routes.

