package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/store"
)

// TenantLookup is the only thing RequireAPIKey needs from the store, so
// tests can pass a fake instead of a database.
type TenantLookup interface {
	ByAPIKeyHash(ctx context.Context, apiKeyHash string) (store.Tenant, error)
}

// TenantFromContext returns the tenant authenticated by RequireAPIKey.
func TenantFromContext(ctx context.Context) (store.Tenant, bool) {
	t, ok := ctx.Value(tenantKey).(store.Tenant)
	return t, ok
}

// RequireAPIKey authenticates "Authorization: Bearer rly_..." and puts the
// tenant in the request context. Every client-side failure gets the same
// 401 body so callers learn nothing about why; the reason only goes to logs.
// The key itself is never logged.
func RequireAPIKey(log *slog.Logger, lookup TenantLookup, env apikey.Env) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reject := func(reason string) {
				log.Info("auth rejected",
					"reason", reason,
					"path", r.URL.Path,
					"request_id", RequestIDFromContext(r.Context()),
				)
				w.Header().Set("WWW-Authenticate", `Bearer realm="relay"`)
				writeError(w, http.StatusUnauthorized, "unauthorized")
			}

			key, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				reject("missing or malformed authorization header")
				return
			}
			// Shape, checksum and env are checked offline: garbage never reaches the DB.
			if !apikey.ValidForEnv(key, env) {
				reject("invalid key format or wrong env")
				return
			}

			tenant, err := lookup.ByAPIKeyHash(r.Context(), apikey.Hash(key))
			switch {
			case err == nil:
			case errors.Is(err, store.ErrNotFound):
				reject("unknown key")
				return
			case errors.Is(err, store.ErrRevoked):
				reject("revoked key")
				return
			case r.Context().Err() != nil:
				// Client went away mid-lookup; nobody is left to answer.
				return
			default:
				// A DB failure is our fault, not the client's: 500, not 401.
				log.Error("auth lookup failed",
					"err", err,
					"request_id", RequestIDFromContext(r.Context()),
				)
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}

			ctx := context.WithValue(r.Context(), tenantKey, tenant)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the token from "Bearer <token>". The scheme is
// case-insensitive (RFC 7235); the token is not.
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// writeError writes {"error": code} with the same shape as the API's own
// error responses, so a 401 from the middleware looks like any other error.
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{code})
}
