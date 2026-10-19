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
