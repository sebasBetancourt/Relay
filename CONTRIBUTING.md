# Contributing to Relay

Thanks for helping. This guide gets you from clone to pull request.

## Set up

Requirements: Go 1.25+, Docker, and [golang-migrate](https://github.com/golang-migrate/migrate) (optional, Compose runs migrations too).

```bash
cp .env.example .env          # set ENCRYPTION_KEY (openssl rand -base64 32) and RELAY_ENV=test
docker compose up -d postgres migrate
go run ./cmd/api              # needs at least one provider in .env; see docs/*-setup.md
```

## Before you push

```bash
go vet ./...
golangci-lint run ./...
TEST_DATABASE_URL='postgres://relay:relay@localhost:5435/relay?sslmode=disable' go test -race -count=1 ./...
```

CI runs the same checks, plus `govulncheck`, the OpenAPI lint (`npx @redocly/cli lint api/openapi.yaml`) and a Docker build.

- **Unit tests** live next to the code they test.
- **Integration tests** (`*_integration_test.go`) need Postgres and skip without `TEST_DATABASE_URL`.
- **End-to-end tests** live in `test/e2e`. They drive the public API against stateful fake providers (`internal/testutil/fakegoogle`, `fakemicrosoft`); no real Google or Microsoft account is ever needed.

## Pull requests

- One topic per PR. Keep refactors separate from behavior changes.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`.
- Behavior changes come with tests. A bug fix comes with the test that would have caught it.
- Public API changes update `api/openapi.yaml`, the README and `CHANGELOG.md` (under `[Unreleased]`).
- Schema changes add a new pair of files in `migrations/` (never edit an applied migration) and must work `up`, `down`, `up`.
- Architectural decisions get an ADR in `docs/adr/`.

## Code layout

See [ADR 0004](docs/adr/0004-project-layout.md) and the "Project layout" section of the README. In short, packages are organized by responsibility, dependencies point one way (`httpapi → calendar → oauth → store`), and interfaces are declared by the package that uses them.

## Adding a provider

Relay is built so a new provider does not touch the OAuth flow:

1. `internal/oauth`: a `ProviderSpec` with the endpoints, scopes, consent options, scope check, `Identify` and, if the provider has one, `Revoke`.
2. `internal/calendar`: a `Source` that lists events and normalizes them to `calendar.Event`.
3. `internal/config`: its variables (all-or-nothing, validated at startup), and the wiring in `cmd/api`.
4. `internal/testutil`: a stateful fake built on `fakeoauth`, and add the provider to the e2e `providers` list.
5. A setup guide in `docs/` and a row in the README providers table.

## Security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
