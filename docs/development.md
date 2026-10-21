# Development

How to build, test and find your way around the code. For contributing rules see [CONTRIBUTING.md](../CONTRIBUTING.md).

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

Packages are organized by responsibility, not by layer ([ADR 0004](adr/0004-project-layout.md)). Everything under `internal/` can only be imported by this module.

```
cmd/
  api/                      API entry point; the only place that reads config and wires packages
  relayctl/                 admin CLI (create tenants, version)
internal/
  config/                   env loading and validation (fail fast)
  httpapi/                  router and handlers: me, connect, callback, calendars, accounts
    middleware/             request id, access log, panic recovery, API key auth
  oauth/                    providers (Google, Microsoft), connect flow, token lifecycle (refresh, rotation, revoke)
  calendar/                 events service + provider sources (Google, Microsoft Graph)
  store/                    Postgres access, SQL only
  apikey/                   API key generation, checksum and hashing
  vault/                    AES-256-GCM encryption with AAD for tokens at rest
  janitor/                  background purge of expired oauth_states
  buildinfo/                version stamped at build time
  testutil/                 stateful fake providers for tests (fakeoauth, fakegoogle, fakemicrosoft)
test/e2e/                   end-to-end flow suite
migrations/                 SQL migrations (golang-migrate)
api/openapi.yaml            OpenAPI 3.1 contract
docs/                       design, ADRs, provider setup guides, API guide
bin/                        build output (gitignored)
```

Dependencies point one way: `httpapi → calendar → oauth → store`. `vault`, `apikey` and `store` import nothing internal. Interfaces are declared by the package that uses them (e.g. `middleware.TenantLookup`, `oauth.StateStore`), so each package can be tested with small fakes.

## Releasing

Releases are cut by pushing a tag; `.github/workflows/release.yml` does the rest.

1. Move the `[Unreleased]` entries of `CHANGELOG.md` under a new `## [X.Y.Z] - YYYY-MM-DD` section. The release notes are taken from that section, and the workflow fails without it.
2. Check that CI is green on `master`.
3. Tag and push:

   ```bash
   git tag -a vX.Y.Z -m "Relay vX.Y.Z"
   git push origin vX.Y.Z
   ```

The workflow then:

- reruns the whole CI;
- builds `relay` and `relayctl` for Linux, macOS and Windows (amd64 and arm64), stamped with the version;
- publishes archives (binaries, migrations, `.env.example`) with `checksums.txt`;
- pushes the multi-arch image `ghcr.io/sebasbetancourt/relay:vX.Y.Z` (and `latest` for stable versions).

To try it locally without publishing anything:

```bash
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOMODCACHE=/tmp/gomod \
  -e GOFLAGS=-buildvcs=false -v "$PWD":/src -w /src goreleaser/goreleaser:v2.12.7 release --snapshot --clean --skip=docker
```
