# ADR 0012: Run on Fly.io with private Postgres and NATS

## Status
Accepted (2026-10-30)

## Context
Webhooks and production redirect URIs need a public HTTPS address, and a demo needs something people can reach without cloning the repository. Relay has no paying users yet, so cost matters more than availability. The API runs background work, and a host in front of it changes what `RemoteAddr` means.

## Options considered
- **A small VPS with Compose and Caddy:** Cheapest, and reuses `docker-compose.yml`. Patching, firewall, disks and backups are ours, and a typo can expose Postgres.
- **Fly.io with its managed Postgres:** Least work, but the Basic plan is about $38 per month, more than the rest of the stack.
- **Fly.io running `postgres:16` on a volume:** TLS and restarts are handled, and an app without an `[http_service]` has no public address. Fly's own unmanaged Postgres is no longer maintained, so this uses the official image.
- **The home server behind a tunnel:** Free, but a power cut takes the OAuth callback down.

## Decision
Relay runs on Fly.io as three apps: the API, plus Postgres and NATS on volumes with no public address. Fly's proxy terminates TLS and renews certificates.

- **No idle stop.** `auto_stop_machines` is off, because token refresh, subscription renewal and the sync consumer live in the API.
- **One machine, no Redis.** With a single API machine a local rate limit is exact, and Redis would add cost for nothing. A second machine needs `REDIS_URL` first.
- **Migrations in the binary.** The image has no shell, so `relay migrate` embeds the SQL and runs as Fly's `release_command`. A failed migration stops the deploy before traffic moves. It uses golang-migrate's `schema_migrations` table, so Compose and this command share a database.
- **The client's address.** Behind a proxy, `RemoteAddr` is the proxy, which would put every client in one rate limit budget, as ADR 0009 warned. `CLIENT_IP_HEADER` names the header to trust, here `Fly-Client-IP`. It stays empty by default, because without a trusted proxy a client could choose its own address.
- **Backups off the platform.** A daily `pg_dump`, encrypted with age, is kept as a workflow artifact. It must be encrypted because artifacts of a public repository are readable. `ENCRYPTION_KEY` is kept out of every backup, so one stolen file cannot open the tokens.

## Consequences
- **Good:** About $8 per month, HTTPS with no certificate work, and databases that cannot be reached from the internet by accident.
- **Bad:** Each piece runs once. A host failure means downtime, and there is no failover for Postgres.
- **Bad:** Postgres is ours to run: upgrades, disk size and restores. Fly's volume snapshots help, but nothing here gives point-in-time recovery, so up to a day of data can be lost.
- **Bad:** There is no free tier, and leaving Fly means moving the database and registering new redirect URIs, unless a custom domain is added first.
- **Neutral:** Workflow artifacts expire after 30 days, which is enough for a demo but not for long retention.
