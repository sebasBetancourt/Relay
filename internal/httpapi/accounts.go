package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sebasBetancourt/relay/internal/httpapi/middleware"
	"github.com/sebasBetancourt/relay/internal/store"
)

const (
	defaultAccountsLimit = 50
	maxAccountsLimit     = 100
)

// AccountLister is what GET /v1/accounts needs (store.Accounts).
type AccountLister interface {
	ListForTenant(ctx context.Context, tenantID string, limit int, after *store.AccountCursor) ([]store.AccountSummary, error)
}

// Disconnector is what DELETE /v1/accounts/{id} needs (oauth.Service).
type Disconnector interface {
	Disconnect(ctx context.Context, tenantID, accountID string) error
}

type accountJSON struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	Email       string    `json:"email,omitempty"`
	Status      string    `json:"status"`
	ConnectedAt time.Time `json:"connected_at"`
}

type accountsResponse struct {
	Accounts   []accountJSON `json:"accounts"`
	NextCursor string        `json:"next_page_token,omitempty"`
}

// cursorJSON is the opaque `page_token` of GET /v1/accounts.
type cursorJSON struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

func handleListAccounts(log *slog.Logger, accounts AccountLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := middleware.TenantFromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal_error"})
			return
		}
		limit, after, detail := parseAccountsQuery(r)
		if detail != "" {
			writeJSON(w, http.StatusBadRequest, queryErrorBody{Error: "invalid_query", Detail: detail})
			return
		}

		// One extra row tells whether another page exists.
		rows, err := accounts.ListForTenant(r.Context(), t.ID, limit+1, after)
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusBadRequest, queryErrorBody{Error: "invalid_query", Detail: "page_token is invalid"})
			return
		}
		if err != nil {
			log.Error("list accounts failed", "err", err, "request_id", middleware.RequestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal_error"})
			return
		}

		resp := accountsResponse{Accounts: make([]accountJSON, 0, min(len(rows), limit))}
		if len(rows) > limit {
			last := rows[limit-1]
			resp.NextCursor = encodeAccountsCursor(cursorJSON{CreatedAt: last.ConnectedAt, ID: last.ID})
			rows = rows[:limit]
		}
		for _, a := range rows {
			resp.Accounts = append(resp.Accounts, accountJSON{
				ID: a.ID, Provider: a.Provider, Email: a.Email, Status: a.Status, ConnectedAt: a.ConnectedAt,
			})
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusOK, resp)
	}
}

func handleDisconnectAccount(log *slog.Logger, d Disconnector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := middleware.TenantFromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal_error"})
			return
		}
		accountID := r.PathValue("id")

		err := d.Disconnect(r.Context(), t.ID, accountID)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, store.ErrNotFound):
			// Same answer for "missing" and "another tenant's": no leak.
			writeJSON(w, http.StatusNotFound, errorBody{Error: "not_found"})
		default:
			log.Error("disconnect failed",
				"err", err,
				"account_id", accountID,
				"request_id", middleware.RequestIDFromContext(r.Context()),
			)
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal_error"})
		}
	}
}

func parseAccountsQuery(r *http.Request) (limit int, after *store.AccountCursor, detail string) {
	qs := r.URL.Query()
	limit = defaultAccountsLimit
	if raw := qs.Get("max_results"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxAccountsLimit {
			return 0, nil, "max_results must be an integer between 1 and 100"
		}
		limit = n
	}
	if raw := qs.Get("page_token"); raw != "" {
		c, ok := decodeAccountsCursor(raw)
		if !ok {
			return 0, nil, "page_token is invalid"
		}
		after = &store.AccountCursor{CreatedAt: c.CreatedAt, ID: c.ID}
	}
	return limit, after, ""
}

func encodeAccountsCursor(c cursorJSON) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeAccountsCursor(s string) (cursorJSON, bool) {
	if len(s) > 512 {
		return cursorJSON{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursorJSON{}, false
	}
	var c cursorJSON
	if err := json.Unmarshal(raw, &c); err != nil || c.ID == "" || c.CreatedAt.IsZero() {
		return cursorJSON{}, false
	}
	return c, true
}
