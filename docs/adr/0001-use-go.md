# ADR 0001: Use Go for the API

## Status
Accepted (2026-10-18)

## Context
Relay is an HTTP API that unifies Google Calendar and Microsoft Graph. It needs solid concurrency (call both providers, later background workers), predictable performance under I/O wait, mature OAuth libraries (`golang.org/x/oauth2`, official MSAL for Go), and a simple production deploy (Docker).

## Options considered
- **Go:** First-class goroutines for lightweight concurrency; strong stdlib, `x/oauth2`, and official MSAL for Go; compiles to a single static binary with no runtime deps.
- **Node/TypeScript:** Already known; huge ecosystem and fast to prototype. Weaker default model for CPU-bound work and long-lived workers; deploy needs a Node runtime and `node_modules`.
- **Python:** Fast to write, good OAuth libs. Heavier runtime and weaker fit for a small, concurrent API service we want to keep lean.

## Decision
We use **Go** because goroutines fit parallel provider calls and future v0.2 workers, the OAuth ecosystem covers Google (`golang.org/x/oauth2`) and Microsoft (official MSAL for Go / `microsoft-authentication-library-for-go`), and a single binary keeps Docker images small and deploys trivial.

## Consequences
- **Good:** Cheap concurrency for Google + Microsoft in parallel; one artifact to ship; clear path to workers without a second language.
- **Cost:** Learning curve for Go (idioms, tooling, error handling) compared to sticking with Node/TypeScript.
