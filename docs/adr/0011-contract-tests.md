# ADR 0011: Tests hold the code to the OpenAPI spec

## Status
Accepted (2026-10-30)

## Context
`api/openapi.yaml` shipped in v0.1.0 and CI lints it, but linting only proves the file is well formed. A new field or route could ship undocumented with CI green. When the checks below first ran, they found exactly that: an undocumented `invalid_query` on connect, webhook bodies the spec said were empty, and an error code missing from a schema.

## Decision
- **Validate traffic, not the file.** `internal/testutil/contract` wraps a test's router with `kin-openapi` and checks every exchange. Handler and end-to-end suites build their routers through it, so existing tests became contract tests, for both providers.
- **Close the schemas in memory.** JSON Schema accepts extra properties by default, so an undocumented field would pass. The checker sets `additionalProperties: false` when it loads the spec. The published file stays open, so clients may ignore fields added later.
- **Check what the library skips.** It does not see undocumented query parameters, a body on a response documented as empty, or a missing `X-Request-ID`, so the wrapper adds those.
- **Judge a request only if the server accepted it.** Tests send bad input on purpose. A request answered below 400 must match the spec; a refused one is not judged. A `401`, `404` or `405` on an unknown path is by design, because auth hides which paths exist.
- **Compare routes both ways.** `newRouter` records its patterns, and `TestRoutesMatchTheSpec` fails on a route served but not documented, or documented but not served.
- **No generated code.** The spec describes the contract and the code stays the source of behavior, so the architecture does not depend on a generator.
- **Publish from the same file.** A workflow builds the reference with Redocly and deploys it to GitHub Pages when the spec changes. The release workflow stops if `info.version` differs from the tag.

## Consequences
- **Good:** The docs cannot drift silently on anything a test exercises. Each new field needs a spec entry to go green.
- **Good:** `kin-openapi` is a test dependency only; the API binary does not include it.
- **Bad:** The checker sees only what tests trigger. An answer no test reaches is not validated, so new errors need a test.
- **Bad:** Bodies of `4xx` requests are not validated, so a documented constraint the handler does not enforce goes unnoticed.
- **Bad:** `kin-openapi` supports OpenAPI 3.1 partially. The spec's `const` and `examples` work today; a newer construct may need checking first.
