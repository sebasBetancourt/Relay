# Contributing to Relay

Thanks for helping. This guide gets you from clone to pull request.

## Set up

Requirements: Go 1.25+, Docker with Compose, and [golangci-lint](https://golangci-lint.run/) v2. No Google or Microsoft account is needed to run the tests.

A weekly job in CI runs the commands below on a clean machine, exactly as they are written here ([`scripts/check-guide.sh`](scripts/check-guide.sh)). The `guide:` comments in this file's source mark those blocks. If you change a step, keep them working.

Create your local config. The first time only, because this replaces `.env`:

<!-- guide:run -->
```bash
cp .env.example .env
printf 'ENCRYPTION_KEY=%s\n' "$(openssl rand -base64 32)" >> .env
printf 'GOOGLE_CLIENT_ID=local\nGOOGLE_CLIENT_SECRET=local\n' >> .env
```

Those placeholder credentials let the API start. To connect a real account, replace them by following [Google setup](docs/google-cloud-setup.md) or [Microsoft setup](docs/microsoft-setup.md).

Start what Relay depends on and apply the migrations:

<!-- guide:run -->
```bash
docker compose up -d --wait postgres nats redis
docker compose run --rm migrate
```

Start the API on `localhost:8080`:

<!-- guide:serve -->
```bash
go run ./cmd/api
```

| Variable | Without it | To turn it on |
| --- | --- | --- |
| `REDIS_URL` | One rate limit budget per replica | `redis://localhost:6380/0` |
| `NATS_URL` | No delta sync or push | `nats://localhost:4222` |
| `WEBHOOK_BASE_URL` | Calendar push is off | A public `https` origin, see below. Needs `NATS_URL` |

Compose publishes Postgres on `5435`, Redis on `6380` and NATS on `4222`. To run everything in containers instead, use `docker compose up -d --build`, which sets `REDIS_URL` and `NATS_URL` for you.

### Testing webhooks

You rarely need a real provider. `test/e2e/webhooks_test.go` drives the whole flow against the fakes, which subscribe, send notifications and renew like the real services, and `internal/notify/contract_test.go` covers every answer.

To see a real notification, Google and Microsoft must reach you over public HTTPS. Point a tunnel at the API, for example with [cloudflared](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/):

```bash
cloudflared tunnel --url http://localhost:8080
```

Set `WEBHOOK_BASE_URL` to the URL it prints and `NATS_URL` to `nats://localhost:4222`, restart the API, and connect an account.

## Before you push

<!-- guide:run -->
```bash
go vet ./...
golangci-lint run ./...
TEST_DATABASE_URL='postgres://relay:relay@localhost:5435/relay?sslmode=disable' \
TEST_REDIS_URL='redis://localhost:6380/0' \
  go test -race -count=1 ./...
npx --yes @redocly/cli@1 lint api/openapi.yaml
```

CI runs the same checks, plus `govulncheck` and a Docker build.

- **Unit tests** live next to the code they test.
- **Integration tests** (`*_integration_test.go`) need Postgres and skip without `TEST_DATABASE_URL`.
- **End-to-end tests** live in `test/e2e`. They drive the public API against stateful fake providers (`internal/testutil/fakegoogle`, `fakemicrosoft`); no real Google or Microsoft account is ever needed.
- **Contract tests** check every response against `api/openapi.yaml`. In `internal/httpapi`, build routers with `contractRouter`. A new route or error needs its entry in the spec and a test that reaches it. See [the API contract](docs/development.md#the-api-contract).

## Pull requests

- One topic per PR. Keep refactors separate from behavior changes.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`.
- Behavior changes come with tests. A bug fix comes with the test that would have caught it.
- Public API changes update `api/openapi.yaml`, the README and `CHANGELOG.md` (under `[Unreleased]`).
- Schema changes add a new pair of files in `migrations/` (never edit an applied migration) and must work `up`, `down`, `up`.
- Architectural decisions get an ADR: copy [the template](docs/adr/template.md) to `docs/adr/` with the next free number.

## Code layout

See [ADR 0004](docs/adr/0004-project-layout.md) and [Project layout](docs/development.md#project-layout). In short, packages are organized by responsibility, dependencies point one way (`httpapi → calendar → oauth → store`), and interfaces are declared by the package that uses them.

## Adding a provider

Relay is built so a new provider does not touch the OAuth flow or the router. The provider must be able to do everything the others do:

1. `internal/oauth`: a `ProviderSpec` with the endpoints, scopes, consent options, scope check, `Identify` and, if the provider has one, `Revoke`. Add its write scope to `featureScopes`.
2. `internal/calendar`: a `Source` that lists events and normalizes them to `calendar.Event`, and a `Writer` that translates an `EventChange`. A write the provider cannot store exactly as the others do must be refused, never approximated ([ADR 0010](docs/adr/0010-event-writes.md)).
3. `internal/sync`: a `DeltaSource` that returns changes since a cursor.
4. `internal/notify`: a `Provider` that subscribes, renews and stops, and its webhook route in `internal/httpapi`, documented in `api/openapi.yaml`.
5. `internal/config`: its variables (all-or-nothing, validated at startup), and the wiring in `cmd/api`.
6. `internal/testutil`: a stateful fake built on `fakeoauth`, and add the provider to the e2e `providers` list.
7. A setup guide in `docs/` and a row in the README features table.

## Security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
