package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/sebasBetancourt/relay/internal/httpapi/middleware"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

// Connector is what the connect handler needs from internal/connect.
type Connector interface {
	Start(ctx context.Context, tenantID string, provider oauth.Provider, features ...oauth.Feature) (string, error)
}

// handleConnect starts an OAuth connect (design.md, Request flow A.1–2).
//
// Default answer is 302 to the consent screen. A tenant backend calling
// with its API key cannot hand that header to a browser, so with
// "Accept: application/json" it gets 200 {"authorization_url": ...} instead
// and forwards the URL to its end user.
func handleConnect(log *slog.Logger, c Connector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := middleware.TenantFromContext(r.Context())
		if !ok {
			// Only possible if the route was mounted without RequireAPIKey.
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal_error"})
			return
		}
		provider := oauth.Provider(r.PathValue("provider"))

		features, ok := parseFeatures(r.URL.Query().Get("features"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, queryErrorBody{Error: "invalid_query", Detail: "features must be a comma-separated list of: calendar.write"})
			return
		}

		authURL, err := c.Start(r.Context(), tenant.ID, provider, features...)
		switch {
		case err == nil:
		case errors.Is(err, oauth.ErrUnsupportedProvider):
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "unsupported_provider"})
			return
		case errors.Is(err, store.ErrNotFound):
			// Tenant deleted while its key was still cached: the key is no longer valid.
			w.Header().Set("WWW-Authenticate", `Bearer realm="relay"`)
			writeJSON(w, http.StatusUnauthorized, errorBody{Error: "unauthorized"})
			return
		default:
			log.Error("connect start failed",
				"err", err,
				"tenant_id", tenant.ID,
				"provider", provider,
				"request_id", middleware.RequestIDFromContext(r.Context()),
			)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal_error"})
			return
		}

		// Never log the URL: it carries the one-time state.
		log.Info("connect started",
			"tenant_id", tenant.ID,
			"provider", provider,
			"request_id", middleware.RequestIDFromContext(r.Context()),
		)
		// The URL holds a one-time state: no proxy or browser may cache it.
		w.Header().Set("Cache-Control", "no-store")

		if strings.Contains(r.Header.Get("Accept"), "application/json") {
			writeJSON(w, http.StatusOK, struct {
				AuthorizationURL string `json:"authorization_url"`
			}{authURL})
			return
		}
		http.Redirect(w, r, authURL, http.StatusFound)
	}
}

func parseFeatures(raw string) ([]oauth.Feature, bool) {
	if raw == "" {
		return nil, true
	}
	var out []oauth.Feature
	for name := range strings.SplitSeq(raw, ",") {
		f, ok := oauth.ParseFeature(strings.TrimSpace(name))
		if !ok {
			return nil, false
		}
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out, true
}
