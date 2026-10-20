// Package middleware holds the HTTP middlewares of the Relay API: request
// id, access log, panic recovery and API key authentication. Handlers read
// what the middlewares put in the context through RequestIDFromContext and
// TenantFromContext.
package middleware

import (
	"context"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	tenantKey
)

func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}
