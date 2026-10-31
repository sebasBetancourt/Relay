# API guide

A walk through every endpoint with `curl`. The formal contract is [api/openapi.yaml](../api/openapi.yaml) (OpenAPI 3.1); paste it into [Swagger Editor](https://editor.swagger.io/) or import it into Postman to browse it.

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

- `/healthz` and `/readyz` are public. The OAuth callback and the provider webhook routes are public too. Every other `/v1/` route requires `Authorization: Bearer <key>`.
- Malformed keys, bad checksums and keys from the other environment are rejected without touching the database.
- Every client error gets the same `401 {"error":"unauthorized"}`; the reason is only logged, never the key itself. A database failure returns `500`, not `401`.
- Revoked keys stop working immediately on the instance that revoked them, and within 30 seconds (cache TTL) on the others.

Key format and secret scanning: [secret-scanning.md](secret-scanning.md).

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
| Database failure | `500 {"error":"internal_error"}` |

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
| DB / vault failure | `500` | `internal_error` |

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

Expired access tokens are refreshed silently: 60s skew, singleflight per account, and a row lock in Postgres so replicas never race (Microsoft rotates the refresh token on every refresh). A background job also renews tokens about 10 minutes before they expire (`REFRESH_AHEAD`, `REFRESH_CONCURRENCY`). If the provider rejects the refresh, the account is marked `revoked` and the client gets **409** (reconnect via `/v1/connect/{provider}`).

| Case | Status | `error` |
| --- | --- | --- |
| OK | 200 | — |
| Invalid query | 400 | `invalid_query` |
| Missing / wrong-tenant account | 404 | `not_found` |
| Account revoked / refresh invalid / MFA required | 409 | `account_revoked` |
| Account disconnected by the tenant | 409 | `account_disconnected` |
| Microsoft account without an Outlook mailbox | 409 | `calendar_unavailable` |
| Relay rate limit | 429 | `rate_limited` (+ `Retry-After`) |
| Provider rate limit | 429 | `provider_rate_limited` (+ `Retry-After`) |
| Provider down / timeout | 502 | `provider_unavailable` |
| Unexpected provider response | 502 | `provider_error` |
| DB / decrypt failure | 500 | `internal_error` |

## Write events

Writing needs more than the read-only connect. Ask for it with `features`, or connect the same account again with it. The account keeps its id:

```bash
curl -s -H "Authorization: Bearer $(cat acme.key)" -H "Accept: application/json" \
  "localhost:8080/v1/connect/google?features=calendar.write" | jq -r .authorization_url
```

Without that consent, a write answers **403** with the way out:

```json
{"error":"insufficient_scope","required_scopes":["https://www.googleapis.com/auth/calendar.events"],"reconnect_url":"/v1/connect/google?features=calendar.write"}
```

The same body works for both providers:

```bash
curl -s -X POST -H "Authorization: Bearer $(cat acme.key)" -H "Content-Type: application/json" -H "Idempotency-Key: $(uuidgen)" \
  "localhost:8080/v1/calendars/$ACCOUNT_ID/events" -d '{
    "title": "Standup",
    "start": "2026-11-02T09:00:00-05:00",
    "end": "2026-11-02T09:30:00-05:00",
    "timezone": "America/Bogota",
    "attendees": ["ana@example.com"],
    "recurrence": {"freq": "WEEKLY", "by_day": ["MO", "WE"], "count": 10}
  }' | jq
```

| Method | Path | Answer |
| --- | --- | --- |
| `POST` | `/v1/calendars/{id}/events` | **201** with the event |
| `PATCH` | `/v1/calendars/{id}/events/{event_id}` | **200** with the event; only the fields sent change |
| `DELETE` | `/v1/calendars/{id}/events/{event_id}` | **204** |

- **Timed events** take `start` and `end` in RFC 3339 with an offset, plus an IANA `timezone`. They keep the same local time on both providers, also across DST.
- **All-day events** take `"all_day": true` and `YYYY-MM-DD`, with `end` exclusive.
- **Recurrence** covers `DAILY`, `WEEKLY` (with `by_day`), `MONTHLY` by day of the month and `YEARLY`, ending with `count`, with `until` (inclusive, in the event's zone) or never. Weeks start on Monday.
- **Attendees get real emails.** Graph always sends invitations and Google is told to as well (`sendUpdates=all`), so both behave the same. Use your own accounts in tests.

Anything the providers would not store identically is refused with `400`, and nothing is created:

| Refused | Why |
| --- | --- |
| Monthly on the 29th, 30th or 31st; yearly on February 29 | Google skips the short month, Outlook moves to the last day |
| `by_day` that does not include the start day | The providers disagree on the first occurrence |
| `by_day` on anything but `WEEKLY`, or `until` with `count` | Outside the supported subset |
| Editing or deleting a single occurrence | Only whole series, using the id returned on create |
| Moving a series without sending its `recurrence` | The new start could fall outside the pattern |
| A zone CLDR does not list (`unsupported_timezone`) | Graph needs a Windows name, and Relay never guesses one. Current names such as `Asia/Kolkata` are mapped; old links such as `Australia/Tasmania` are not |

`GET /events` expands series, so its ids belong to single occurrences. Keep the id returned on create to change a series.

**Safe retries.** With an `Idempotency-Key`, repeating a request returns the stored answer for 24 hours, marked `Idempotent-Replayed: true`, and writes nothing. The same key with another route or body is **422**, and while the first request runs it is **409** with `Retry-After`. Only successes are stored, so a failed request can be retried with the same key. If a provider times out, the event may exist even though Relay answered `502`, and a retry can create it twice.

**Concurrent edits.** Every event carries a `version`. Send it as `If-Match` on `PATCH`, and if someone changed the event since, the answer is **412** `version_conflict` and nothing changes.

## Compatibility

From v1.0, `/v1` follows [semantic versioning](https://semver.org/). A breaking change only happens in a new major version, under a new path (`/v2`). The contract is [`api/openapi.yaml`](../api/openapi.yaml), and tests check every response against it.

**Not breaking**, so these can arrive in any minor version. Write your client to tolerate them:

- new endpoints, and new optional parameters or body fields;
- new fields in a response;
- new `error` codes, on a status a route already documents or on a new failure;
- new values in `status`, `provider` and other strings a client reads;
- higher limits.

**Breaking**, so these only happen in `/v2`:

- removing or renaming an endpoint, parameter or response field;
- changing a field's type or meaning, or the format of a value;
- making an optional input required;
- changing the status or the `error` code of an existing case;
- refusing a request that used to be accepted, unless it was a bug or a security problem.

Some things are not part of the contract:

- the text of `detail`, which is for people: match on `error`, never on `detail`;
- the contents of `page_token`, which is opaque and only valid for the paging it came from;
- the exact budget of a rate limit, which depends on the deployment. The format of the `RateLimit` headers follows an IETF draft and may follow it as it changes;
- the set of accepted recurrences and time zones may grow, so a request refused today may be accepted later;
- an event's `version`, which you can only compare or send back as `If-Match`.

## Rate limits

Every authenticated `/v1` request spends from the tenant's budget, shared by all replicas. Past it, Relay answers **429**:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 1
RateLimit-Policy: "tenant";q=600;w=60
RateLimit: "tenant";r=0;t=1

{"error":"rate_limited"}
```

Allowed responses carry the same `RateLimit-Policy` and `RateLimit` headers (`r` is what remains, `t` the seconds until the budget is full), so a client can slow down before it is refused.

| Who | Counts against | Default | Variables |
| --- | --- | --- | --- |
| Valid API key | The tenant | 600 per minute, burst 100 | `RATE_LIMIT_PER_MINUTE`, `RATE_LIMIT_BURST` |
| OAuth callback, or a request without a valid key | The client IP | 1200 per minute, burst 200 | `RATE_LIMIT_IP_PER_MINUTE`, `RATE_LIMIT_IP_BURST` |

- `/healthz` and `/readyz` are never limited. The provider webhooks have their own per-IP limit.
- A request without a valid key gets its usual `401` until the IP budget is spent, then `429`.
- Give one tenant its own limit in SQL. `NULL` goes back to the global value, and the change reaches every replica within 30 seconds. The burst is capped to the override:

  ```sql
  UPDATE tenants SET rate_limit_per_minute = 6000 WHERE id = '<tenant id>';
  ```

- This protects Relay and its tenants from each other, not the provider quotas. A provider's own throttling still arrives as `provider_rate_limited`.

## Manage connected accounts

```bash
# List the tenant's accounts (no tokens, ever)
curl -s -H "Authorization: Bearer $(cat acme.key)" "localhost:8080/v1/accounts?max_results=50" | jq
# {"accounts":[{"id":"…","provider":"microsoft","email":"ana@outlook.com","status":"active","connected_at":"…"}],"next_page_token":"…"}

# Disconnect one
curl -i -X DELETE -H "Authorization: Bearer $(cat acme.key)" localhost:8080/v1/accounts/<account_id>
# 204 No Content
```

- `GET /v1/accounts`: `max_results` 1–100 (default 50). Pass `page_token=<next_page_token>` for the next page; `next_page_token` is absent on the last page. `status` is `active`, `revoked` (the provider rejected the refresh token) or `disconnected`.
- `DELETE /v1/accounts/{id}`: stops the calendar push subscription (best effort), deletes the tokens and the stored calendar copy, and marks the account `disconnected` (204). It is idempotent and the account can be connected again.
  - Google tokens are also revoked at Google (best effort; a Google outage never blocks the disconnect).
  - Microsoft has no per-app revocation endpoint: users remove the app at account.microsoft.com → Privacy → Apps and services.
- Another tenant's account, or one that does not exist, gives `404 not_found` on both routes.

## Calendar push

When `WEBHOOK_BASE_URL` is set (a public `https` origin; loopback `http` is accepted for local tests), connecting an account also subscribes Relay to that calendar. Google gets a watch channel; Microsoft Graph gets a subscription on `me/events` (at most 10,080 minutes). The plaintext channel token and `clientState` are never stored, only a SHA-256. Without `WEBHOOK_BASE_URL` the API logs that push is off and keeps serving events on demand. `NATS_URL` is required when push is on. When `NATS_URL` is set on its own, delta sync still runs and push stays off. A job that still fails after its retries is kept on the dead-letter stream; `relayctl dlq list` shows it and `relayctl dlq replay <seq>` queues it again.

These routes are public. They check the per-subscription secret, publish `relay.sync.account` (or a lifecycle job) and return. They do not call the provider.

| Method | Path | What it does |
| --- | --- | --- |
| `POST` | `/v1/webhooks/google` | `202` after a real change. `sync` and unknown channels are `200` and queue nothing. A bad channel token is `401`. |
| `POST` | `/v1/webhooks/microsoft` | `?validationToken=` answers `200` `text/plain` with that exact token. Otherwise one sync job per account, or `401` if any `clientState` is wrong. |
| `POST` | `/v1/webhooks/microsoft/lifecycle` | `reauthorizationRequired` renews, `subscriptionRemoved` subscribes again, `missed` enqueues a sync. Same handshake. |

A background pass renews anything that expires within 24 hours (Graph: `PATCH`; Google: a new channel, then `channels.stop` on the old one). A provider `5xx` leaves the row in place and the next pass retries. Duplicate deliveries of the same notification collapse to one JetStream message (`Nats-Msg-Id`). The body is limited to 64 KiB, and each address is limited to 120 requests a minute. Per-tenant limits are still to come.

Notifications only mean "sync this account". They are not a list of changes. Delta sync ([ADR 0008](adr/0008-delta-sync.md)) turns each job into the added, updated and removed events since the stored cursor. `GET /v1/calendars/{id}/events` still reads the provider; the stored copy is not the read API.

## Delta sync

Connecting an account enqueues its first sync. A worker also enqueues active accounts whose last sync is older than 15 minutes, so a missed webhook still lands. Google uses a `syncToken`. Microsoft Graph uses `calendarView/delta`, and Relay follows `@odata.nextLink` only when the host is the configured Graph host. An expired cursor is HTTP 410: Relay throws it away and does a full sync of the same window. A different window is a full sync too, because both providers bind the cursor to the original range (30 days, the same default as the events read).

Each event is upserted by `(account_id, provider_event_id)` and rewritten only when its etag or `changeKey` changed. A removal sets `deleted` instead of dropping the row. The new cursor is committed in that same transaction. One account syncs at a time (`SELECT … FOR UPDATE`). Disconnect deletes the copy and the cursor. The cursor is never logged.
