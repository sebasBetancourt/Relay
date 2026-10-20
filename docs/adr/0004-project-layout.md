# ADR 0004: Package layout by responsibility

## Status
Accepted (2026-10-20)

## Context
After the first week (API keys, OAuth connect, callback, events, janitor) the dependency graph was already acyclic and everything lived under `internal/`, but four things made the code harder to navigate:

1. `internal/http` was one flat package with 24 files mixing the router, generic middlewares, API key auth, JSON helpers, four handler groups and their tests.
2. Its name collided with `net/http`: `package http` imported `"net/http"`, and `cmd/api` had to alias it as `relayhttp`.
3. `internal/connect` ran both the connect flow (`Start`/`Finish`) and the whole token lifecycle (decrypt, refresh, revoke), so its name described only half of it.
4. The end-to-end flow suite sat next to the HTTP unit tests although it exercises the whole system.

A layer-based layout (`services/`, `routes/`, `types/`, `pkg/utils/`, every test under `test/`) was also considered.

## Options considered
- **Keep as is:** no work, but every new endpoint and provider grows `internal/http`.
- **Layer-based layout** (`services/`, `routes/`, `types/`, `pkg/utils/`, `test/` for all tests): familiar from other ecosystems, but it fights Go's tooling:
  - A shared `types` package becomes a dependency hub that invites import cycles, and it moves interfaces away from their consumers.
  - `util`-style package names are discouraged by the Go team ("Package names", go.dev/blog).
  - Unit tests outside the package cannot reach unexported code (e.g. `Tenants.cacheGet`).
  - `pkg/` promises an importable API that no other module uses yet. The official guide for server projects (go.dev/doc/modules/layout) uses `internal/` and does not require `pkg/`.
- **Minimal reorganization by responsibility:** two renames, one subpackage, move the e2e suite, add a `bin/` build output.

## Decision
Organize packages by what they do, not by layer:

```
cmd/api, cmd/relayctl           entry points (composition root in cmd/api)
internal/config
internal/httpapi                router, handlers (me, connect, callback, calendars), respond.go
internal/httpapi/middleware     request id, access log, recover, API key auth + context accessors
internal/oauth                  connect flow (flow.go), token lifecycle (tokens.go), providers
internal/calendar               calendar service + provider sources
internal/store                  SQL only
internal/apikey, internal/vault, internal/janitor
internal/testutil/fakegoogle    stateful fake Google for tests
test/e2e                        black-box flow suite (exported APIs only)
bin/                            `make build` output, gitignored
```

Rules that stay:
- Interfaces are declared by the consumer (`middleware.TenantLookup`, `oauth.StateStore`, `calendar.TokenProvider`).
- Request and response payloads live next to their handler.
- Unit tests live next to the code they test.

## Consequences
- **Good:**
  - Each concern has an obvious home.
  - Adding Microsoft touches `oauth/providers.go` and a new `calendar` source.
  - `cmd/api` imports `httpapi` without an alias.
  - The e2e suite fails when the public JSON contract changes, because it declares its own response types.
- **Cost:** handlers import `middleware` to read the tenant and request id; one-time import churn.
- **Revisit:**
  - a small `domain` package if entities (Tenant, Account, Event) spread too far beyond `store`;
  - `pkg/vault` if another module needs it;
  - build tags for integration tests if they get slow.
