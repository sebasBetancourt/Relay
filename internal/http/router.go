package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sebasBetancourt/relay/internal/apikey"
)

func NewRouter(log *slog.Logger, db *pgxpool.Pool, tenants TenantLookup, env apikey.Env) http.Handler {
	mux := http.NewServeMux()

	// Public: probes from Docker/Kubernetes carry no API key.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			log.Warn("readyz failed", "err", err)
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	// Private: everything under /v1/ requires an API key. Auth runs before
	// routing, so unknown /v1 paths also answer 401 without a key and do
	// not reveal which routes exist.
	v1 := http.NewServeMux()
	v1.HandleFunc("GET /v1/me", handleMe)
	mux.Handle("/v1/", RequireAPIKey(log, tenants, env)(v1))

	// Outer runs first. RequestID must wrap Recover/Logger so they see the id in context.
	return RequestID(Recover(log)(Logger(log)(mux)))
}

// handleMe returns the calling tenant: the simplest end-to-end check that
// an API key works.
func handleMe(w http.ResponseWriter, r *http.Request) {
	t, ok := TenantFromContext(r.Context())
	if !ok {
		// Only possible if the route was mounted without RequireAPIKey.
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		KeyPrefix string `json:"key_prefix"`
	}{t.ID, t.Name, t.KeyPrefix})
}
