# Relay
Unified API for Google and Microsoft calendars and mail: connect an account once, then read, write and receive real-time changes through the same API regardless of the provider.

## Stack

**HTTP:** Go standard library [`net/http`](https://pkg.go.dev/net/http) (with Go 1.22+ method-aware routing on `ServeMux`).

We use `net/http` instead of a framework (Gin, Echo, Chi, etc.) because:

- Zero extra dependency — the router and server stay in the stdlib.
- Enough for this API: path/method routing, middleware as plain `http.Handler` wrappers, and clear control over status codes and bodies.
- Keeps the binary and Docker image lean, consistent with shipping a single Go artifact.
- Avoids learning and locking into framework conventions we do not need yet.
