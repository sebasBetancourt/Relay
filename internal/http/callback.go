package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sebasBetancourt/relay/internal/connect"
	"github.com/sebasBetancourt/relay/internal/store"
)

// Callbacker is what the OAuth callback handler needs from connect.
type Callbacker interface {
	Finish(ctx context.Context, provider connect.Provider, state, code, providerErr string) (connect.Result, error)
}

// handleCallback completes Google/Microsoft OAuth (design.md A.3–5).
// Public route: the browser returns from the consent screen without an API key.
// Security is the one-time state (random, 10 min TTL, tenant-bound).
func handleCallback(log *slog.Logger, c Callbacker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provider := connect.Provider(r.PathValue("provider"))
		q := r.URL.Query()
		state := q.Get("state")
		code := q.Get("code")
		providerErr := q.Get("error")

		if state == "" || (code == "" && providerErr == "") {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "invalid_request"})
			return
		}

		res, err := c.Finish(r.Context(), provider, state, code, providerErr)
		if err != nil {
			status, code := mapCallbackError(err)
			if status >= 500 {
				log.Error("oauth callback failed",
					"err", err,
					"provider", provider,
					"request_id", RequestIDFromContext(r.Context()),
				)
			} else {
				log.Info("oauth callback rejected",
					"reason", code,
					"provider", provider,
					"request_id", RequestIDFromContext(r.Context()),
				)
			}
			writeJSON(w, status, errorBody{Error: code})
			return
		}

		log.Info("oauth connected",
			"provider", res.Provider,
			"account_id", res.AccountID,
			"request_id", RequestIDFromContext(r.Context()),
		)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, struct {
			Status    string `json:"status"`
			Provider  string `json:"provider"`
			AccountID string `json:"account_id"`
		}{"connected", string(res.Provider), res.AccountID})
	}
}

func mapCallbackError(err error) (status int, code string) {
	switch {
	case errors.Is(err, connect.ErrUnsupportedProvider):
		return http.StatusBadRequest, "unsupported_provider"
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrStateExpired):
		return http.StatusBadRequest, "invalid_state"
	case errors.Is(err, connect.ErrConsentDenied):
		return http.StatusBadRequest, "access_denied"
	case errors.Is(err, connect.ErrExchangeRejected):
		return http.StatusBadRequest, "exchange_failed"
	case errors.Is(err, connect.ErrInsufficientScope):
		return http.StatusBadRequest, "insufficient_scope"
	case errors.Is(err, connect.ErrInvalidIDToken):
		return http.StatusBadGateway, "provider_error"
	case errors.Is(err, connect.ErrProviderUnavailable):
		return http.StatusBadGateway, "provider_unavailable"
	case errors.Is(err, store.ErrAccountTaken):
		return http.StatusConflict, "account_already_connected"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}
