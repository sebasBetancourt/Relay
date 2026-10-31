# Changelog

All notable changes to Relay are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Deployment on Fly.io: `fly.toml` for the API, private Postgres and NATS, `docs/deploy.md`, and a daily encrypted backup workflow ([ADR 0012](docs/adr/0012-deploy-on-fly.md)).
- `relay migrate` applies the embedded migrations, so an image with no shell can migrate its database.
- `CLIENT_IP_HEADER` names the header a trusted proxy puts the client address in, so rate limits by IP work behind a proxy.
- Contract tests: every response in the handler and end-to-end suites is validated against `api/openapi.yaml`, undocumented fields included, and a test compares the router's routes with the spec ([ADR 0011](docs/adr/0011-contract-tests.md)).
- The API reference is published to GitHub Pages from the spec.
- Write events: `POST /v1/calendars/{id}/events`, plus `PATCH` and `DELETE` on `/{event_id}`, with one body for both providers. Recurrences become `RRULE` or `patternedRecurrence`, and IANA zones become Windows names from CLDR. What the providers cannot store identically is refused with `400`. `Idempotency-Key` makes retries safe, and `If-Match` with the new `version` field refuses stale edits ([ADR 0010](docs/adr/0010-event-writes.md)).
- `GET /v1/connect/{provider}?features=calendar.write` asks for the write scope. A write without it answers `403 insufficient_scope` with a `reconnect_url`.
- Rate limits: each tenant has a budget on `/v1`, enforced with GCRA in Redis so it holds across replicas, with a per-tenant override (`tenants.rate_limit_per_minute`). The OAuth callback and requests without a valid key count per IP. Over the limit, clients get `429 rate_limited` with `Retry-After` and `RateLimit` headers. If Redis fails or `REDIS_URL` is unset, a local limit takes over ([ADR 0009](docs/adr/0009-rate-limiting.md)).
- Durable jobs on NATS JetStream (`RELAY_JOBS`): dedupe by `Nats-Msg-Id`, backoff, and a dead-letter stream (`RELAY_DLQ`) after five attempts. `relayctl dlq list` and `relayctl dlq replay` inspect and requeue them. Async work stays off, and the API says so, when `NATS_URL` is unset ([ADR 0003](docs/adr/0003-use-nats-jetstream.md)).
- Calendar push: connecting an account subscribes to a Google watch channel or a Microsoft Graph subscription. Public webhooks verify a per-subscription secret, enqueue `relay.sync.account` on NATS JetStream and answer inside Graph's 3 second window. A worker renews subscriptions that expire within 24 hours. Push stays off when `WEBHOOK_BASE_URL` is unset ([ADR 0007](docs/adr/0007-calendar-push.md)).
- Delta sync: a consumer applies Google `syncToken` and Graph `deltaLink` changes idempotently, commits the new cursor in the same transaction, and full-syncs on HTTP 410. A 15-minute pass covers missed webhooks. `GET /v1/calendars/{id}/events` still reads the provider ([ADR 0008](docs/adr/0008-delta-sync.md)).

### Fixed

- The spec now documents `invalid_query` on `GET /v1/connect/{provider}`, and the webhooks' plain-text `429` and `503`, their `400` and the `lifecycle` bodies.
- `/healthz`, `/readyz` and empty Graph webhook answers set their `Content-Type` instead of leaving it to sniffing.
- A too-long `Idempotency-Key` answers `400 invalid_idempotency_key`.

## [0.1.0] - 2026-10-20

First public release: one API for Google Calendar and Outlook.

### Added

- **Tenants and API keys**
  - `relayctl tenant create` issues `rly_live_…` / `rly_test_…` keys with a CRC32 checksum.
  - Only the SHA-256 hash is stored, and a key is shown once.
  - Keys can be revoked, with a short in-memory cache.
- **Connect accounts** with `GET /v1/connect/{provider}` (Google and Microsoft):
  - authorization code + PKCE (S256) and a one-time `state` (10 min, deleted on use);
  - `302` to the consent screen, or `200` JSON with `Accept: application/json`.
- **OAuth callback** `GET /v1/oauth/{provider}/callback`:
  - code exchange, scope check and account identity (Google `id_token` `sub`, Microsoft Graph `/me` id);
  - account email stored.
- **Tokens encrypted at rest** with AES-256-GCM.
  - Every cell is bound to tenant, provider account and column through AAD.
  - The key never touches the database.
- **Events** with `GET /v1/calendars/{id}/events`: the primary calendar of Google and Outlook in one normalized shape.
  - Recurrences are expanded.
  - Windows time zones are mapped to IANA (CLDR).
  - Cancelled Outlook occurrences are dropped.
  - Opaque, provider-neutral `page_token`, with no SSRF through Graph `nextLink`.
- **Silent token refresh** (60s skew), safe across replicas:
  - per-process singleflight plus a Postgres row lock;
  - Microsoft's rotated refresh token is stored in the same transaction.
- **Revocation handling**: `invalid_grant` and `interaction_required` mark the account `revoked` (`409 account_revoked`); reconnecting reactivates it.
- **Accounts**:
  - `GET /v1/accounts` with keyset paging;
  - `DELETE /v1/accounts/{id}` with tokens deleted, the Google token revoked best effort, idempotent.
- **Optional providers**: configure Google, Microsoft or both.
  - Strict config validation, all errors reported at startup.
  - Warning when the Microsoft client secret is about to expire.
- **Janitor** that purges expired OAuth states in batches (`FOR UPDATE SKIP LOCKED`).
- **Operations**:
  - distroless images for `linux/amd64` and `linux/arm64` on GHCR;
  - binaries for Linux, macOS and Windows;
  - `relay --version` / `relayctl version`;
  - a Compose stack that applies migrations on start.
- **Docs**:
  - OpenAPI 3.1 spec (`api/openapi.yaml`);
  - Google Cloud and Microsoft Entra ID setup guides;
  - ADRs 0001–0005.
- **Tests**: stateful fake Google and Microsoft (PKCE, one-time codes, strict refresh-token rotation), plus end-to-end suites run for both providers against Postgres.

### Security

- Requests from clients never reach a provider host they chose: the provider base URLs are fixed by config.
- All client errors on `/v1` return the same `401` body; API keys, tokens, states and verifiers are never logged.
- `golang.org/x/text` upgraded to v0.41.0 (GO-2026-6629, GO-2026-5970).

[Unreleased]: https://github.com/sebasBetancourt/Relay/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/sebasBetancourt/Relay/releases/tag/v0.1.0
