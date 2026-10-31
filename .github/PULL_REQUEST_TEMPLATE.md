## What and why

<!-- What does this change, and what problem does it solve? Link the issue. -->

## How it was tested

<!-- Unit / integration / e2e tests added or updated; manual checks. -->

## Checklist

- [ ] `go vet`, `golangci-lint` and `go test -race ./...` (with `TEST_DATABASE_URL`) pass
- [ ] Tests cover the change (a bug fix includes the test that would have caught it)
- [ ] Public API changes update `api/openapi.yaml`, the README and `CHANGELOG.md` (`[Unreleased]`)
- [ ] The contract tests pass: new routes and responses are in the spec, and a test exercises them
- [ ] New migrations work `up`, `down`, `up`; applied migrations are untouched
- [ ] No secrets, tokens or personal data in code, tests, logs or this PR
