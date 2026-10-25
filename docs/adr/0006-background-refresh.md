# ADR 0006: Background token refresh stays on the Postgres row lock

## Status
Accepted (2026-10-24)

## Context
Access tokens were refreshed only inside the client request, when they expired within 60 seconds. That is correct across replicas (`SELECT … FOR UPDATE` on `oauth_tokens`, plus singleflight inside one process), but the first request after expiry waits on the provider, and a revoked refresh token is noticed only when someone asks for data.

ADR 0003 leaves Redis for locks and rate limiting. A leader lock (`SET key NX PX`) could limit scanning to one replica.

## Decision
A process in the API (`internal/refresher`) renews tokens that expire within `REFRESH_AHEAD` (default 10 minutes).

- Each cycle claims one batch with `FOR UPDATE SKIP LOCKED`, ordered by `expires_at`, so two replicas do not take the same row.
- The provider call goes through the existing `oauth.Service` refresh, including the row lock. The claim transaction commits before that call, so the two locks never nest.
- Redis is not used. If a second replica still races a lazy refresh, the row lock is what prevents a double Microsoft rotation. A cache outage must not stop renewal or sign users out.

## Consequences
- **Good:** Requests usually find a live token. Revoked accounts (`invalid_grant`, `interaction_required`) are marked without a client request. Provider 5xx does not revoke; the next cycle retries. Concurrency is capped (`REFRESH_CONCURRENCY`, default 8).
- **Cost:** Every replica scans. That is cheap next to the provider call, and `SKIP LOCKED` drops duplicates. A Redis leader election can be added later without changing the refresh itself.
