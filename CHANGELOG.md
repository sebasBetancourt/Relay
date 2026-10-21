# Changelog

All notable changes to Relay are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
