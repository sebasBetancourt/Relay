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
cp .env.example .env
docker compose up -d --build
make migrate-up
curl http://localhost:8080/healthz   # ok
curl http://localhost:8080/readyz    # ready
```

Notes:

- Postgres is published on host port **5435** (`5435:5432`); `DATABASE_URL` in `.env` and `make migrate-up` use that.
- Inside Compose, the API talks to Postgres as `postgres:5432`.
- If host port **8080** is already in use, stop the other process or change the API port mapping in `docker-compose.yml`.
