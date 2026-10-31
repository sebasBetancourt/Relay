package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/httpapi/middleware"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

const (
	maxEventBody      = 64 << 10 // far above a real event; the provider refuses more anyway
	maxIdempotencyKey = 255
)

// CalendarWriter is what the write handlers need from calendar.Service.
type CalendarWriter interface {
	CreateEvent(ctx context.Context, tenantID, accountID string, in calendar.EventInput) (calendar.Event, error)
	UpdateEvent(ctx context.Context, tenantID, accountID, eventID string, p calendar.EventPatch, version string) (calendar.Event, error)
	DeleteEvent(ctx context.Context, tenantID, accountID, eventID string) error
}

// IdempotencyStore keeps finished writes. *store.Idempotency implements it.
type IdempotencyStore interface {
	Begin(ctx context.Context, tenantID, key, requestHash string) (*store.StoredResponse, error)
	Complete(ctx context.Context, tenantID, key string, res store.StoredResponse) error
	Release(ctx context.Context, tenantID, key string) error
}

type insufficientScopeBody struct {
	Error          string   `json:"error"`
	RequiredScopes []string `json:"required_scopes"`
	ReconnectURL   string   `json:"reconnect_url"`
}

// writeOutcome is a write's answer before it reaches the client, so a success
// can be stored for a replay.
type writeOutcome struct {
	status     int
	body       any // nil for an empty response
	retryAfter int // seconds
}

func handleCreateEvent(log *slog.Logger, cal CalendarWriter, keys IdempotencyStore) http.HandlerFunc {
	return handleWrite(log, keys, func(r *http.Request, tenantID string, body []byte) writeOutcome {
		var in calendar.EventInput
		if err := decodeStrict(body, &in); err != nil {
			return writeOutcome{status: http.StatusBadRequest, body: queryErrorBody{Error: "invalid_body", Detail: err.Error()}}
		}
		ev, err := cal.CreateEvent(r.Context(), tenantID, r.PathValue("id"), in)
		if err != nil {
			return writeFailure(log, r, err)
		}
		return writeOutcome{status: http.StatusCreated, body: ev}
	})
}

func handleUpdateEvent(log *slog.Logger, cal CalendarWriter, keys IdempotencyStore) http.HandlerFunc {
	return handleWrite(log, keys, func(r *http.Request, tenantID string, body []byte) writeOutcome {
		var p calendar.EventPatch
		if err := decodeStrict(body, &p); err != nil {
			return writeOutcome{status: http.StatusBadRequest, body: queryErrorBody{Error: "invalid_body", Detail: err.Error()}}
		}
		ev, err := cal.UpdateEvent(r.Context(), tenantID, r.PathValue("id"), r.PathValue("event_id"), p, parseIfMatch(r.Header.Get("If-Match")))
		if err != nil {
			return writeFailure(log, r, err)
		}
		return writeOutcome{status: http.StatusOK, body: ev}
	})
}

func handleDeleteEvent(log *slog.Logger, cal CalendarWriter, keys IdempotencyStore) http.HandlerFunc {
	return handleWrite(log, keys, func(r *http.Request, tenantID string, _ []byte) writeOutcome {
		if err := cal.DeleteEvent(r.Context(), tenantID, r.PathValue("id"), r.PathValue("event_id")); err != nil {
			return writeFailure(log, r, err)
		}
		return writeOutcome{status: http.StatusNoContent}
	})
}

// handleWrite reads the body and runs fn under the Idempotency-Key, if any.
//
// Only a success is stored. A failure releases the key so the client can
// retry it, which matches the cases where nothing was written. The exception
// is a provider timeout: the event may exist, and a retry can repeat it.
func handleWrite(log *slog.Logger, keys IdempotencyStore, fn func(*http.Request, string, []byte) writeOutcome) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := middleware.TenantFromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBody))
		if err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: "body_too_large"})
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")

		key := r.Header.Get("Idempotency-Key")
		if key == "" || keys == nil {
			writeOutcomeResponse(w, fn(r, t.ID, body))
			return
		}
		if len(key) > maxIdempotencyKey {
			writeJSON(w, http.StatusBadRequest, queryErrorBody{Error: "invalid_query", Detail: "Idempotency-Key must be at most 255 characters"})
			return
		}

		stored, err := keys.Begin(r.Context(), t.ID, key, requestHash(r, body))
		switch {
		case errors.Is(err, store.ErrIdempotencyMismatch):
			writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: "idempotency_key_reused"})
			return
		case errors.Is(err, store.ErrIdempotencyInProgress):
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusConflict, errorBody{Error: "idempotency_in_progress"})
			return
		case err != nil:
			log.Error("idempotency begin failed", "err", err, "request_id", middleware.RequestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
			return
		case stored != nil:
			w.Header().Set("Idempotent-Replayed", "true")
			writeStored(w, *stored)
			return
		}

		out := fn(r, t.ID, body)
		// The client may have gone; the record must still be written.
		ctx := context.WithoutCancel(r.Context())
		if out.status >= 300 {
			if err := keys.Release(ctx, t.ID, key); err != nil {
				log.Error("idempotency release failed", "err", err, "request_id", middleware.RequestIDFromContext(r.Context()))
			}
			writeOutcomeResponse(w, out)
			return
		}
		res := store.StoredResponse{Status: out.status}
		if out.body != nil {
			if res.Body, err = json.Marshal(out.body); err != nil {
				log.Error("idempotency encode failed", "err", err, "request_id", middleware.RequestIDFromContext(r.Context()))
			}
		}
		if err := keys.Complete(ctx, t.ID, key, res); err != nil {
			// The write happened: answer it. A retry after a minute may repeat it.
			log.Error("idempotency complete failed", "err", err, "request_id", middleware.RequestIDFromContext(r.Context()))
		}
		writeOutcomeResponse(w, out)
	}
}

func writeOutcomeResponse(w http.ResponseWriter, out writeOutcome) {
	if out.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(out.retryAfter))
	}
	if out.body == nil {
		w.WriteHeader(out.status)
		return
	}
	writeJSON(w, out.status, out.body)
}

func writeStored(w http.ResponseWriter, res store.StoredResponse) {
	if res.Body == nil {
		w.WriteHeader(res.Status)
		return
	}
	writeJSON(w, res.Status, json.RawMessage(res.Body))
}

// writeFailure maps an error from the calendar service.
func writeFailure(log *slog.Logger, r *http.Request, err error) writeOutcome {
	var (
		invalid *calendar.ValidationError
		scope   *calendar.InsufficientScopeError
	)
	switch {
	case errors.As(err, &invalid):
		return writeOutcome{status: http.StatusBadRequest, body: queryErrorBody{Error: invalid.Code, Detail: invalid.Detail}}
	case errors.As(err, &scope):
		return writeOutcome{status: http.StatusForbidden, body: insufficientScopeBody{
			Error:          "insufficient_scope",
			RequiredScopes: scope.Required,
			ReconnectURL:   "/v1/connect/" + scope.Provider + "?features=" + string(oauth.FeatureCalendarWrite),
		}}
	case errors.Is(err, calendar.ErrVersionConflict):
		return writeOutcome{status: http.StatusPreconditionFailed, body: errorBody{Error: "version_conflict"}}
	}

	status, code, retryAfter := mapCalendarError(err)
	if status >= 500 {
		log.Error("write event failed",
			"err", err,
			"account_id", r.PathValue("id"),
			"request_id", middleware.RequestIDFromContext(r.Context()),
		)
	}
	return writeOutcome{status: status, body: errorBody{Error: code}, retryAfter: int(retryAfter.Seconds())}
}

// requestHash covers the route as well as the body: a key sent to create one
// event must not replay on a delete.
func requestHash(r *http.Request, body []byte) string {
	h := sha256.New()
	h.Write([]byte(r.Method + " " + r.URL.Path + "\n"))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// decodeStrict refuses unknown fields: a typo like "atendees" must not create
// an event without its guests.
func decodeStrict(body []byte, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("body must be a JSON event: " + err.Error())
	}
	return nil
}

// parseIfMatch accepts the version as returned, or the quoted and weak forms an
// HTTP client may send. "*" means any version, the same as no condition.
func parseIfMatch(raw string) string {
	v := strings.TrimPrefix(strings.TrimSpace(raw), "W/")
	v = strings.Trim(v, `"`)
	if v == "*" {
		return ""
	}
	return v
}
