package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/httpapi/middleware"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

const (
	defaultMaxResults = 50
	maxMaxResults     = 250
	defaultWindow     = 30 * 24 * time.Hour
	maxWindow         = 366 * 24 * time.Hour
)

// CalendarReader is what the events handler needs from calendar.Service.
type CalendarReader interface {
	ListEvents(ctx context.Context, tenantID, accountID string, q calendar.Query) (calendar.Page, error)
}

type eventsResponse struct {
	Events        []calendar.Event `json:"events"`
	NextPageToken string           `json:"next_page_token,omitempty"`
}

type queryErrorBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func handleListEvents(log *slog.Logger, cal CalendarReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := middleware.TenantFromContext(r.Context())
		if !ok {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
			return
		}
		accountID := r.PathValue("id")
		q, detail, err := parseEventsQuery(r, time.Now().UTC())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, queryErrorBody{Error: "invalid_query", Detail: detail})
			return
		}

		page, err := cal.ListEvents(r.Context(), t.ID, accountID, q)
		if err != nil {
			if errors.Is(err, calendar.ErrInvalidPageToken) {
				writeJSON(w, http.StatusBadRequest, queryErrorBody{Error: "invalid_query", Detail: "page_token is invalid"})
				return
			}
			status, code, retryAfter := mapCalendarError(err)
			if status >= 500 {
				log.Error("list events failed",
					"err", err,
					"account_id", accountID,
					"request_id", middleware.RequestIDFromContext(r.Context()),
				)
			} else {
				log.Info("list events rejected",
					"reason", code,
					"account_id", accountID,
					"request_id", middleware.RequestIDFromContext(r.Context()),
				)
			}
			if retryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			}
			writeJSON(w, status, errorBody{Error: code})
			return
		}

		if page.Events == nil {
			page.Events = []calendar.Event{}
		}
		w.Header().Set("Cache-Control", "private, no-store")
		writeJSON(w, http.StatusOK, eventsResponse{
			Events:        page.Events,
			NextPageToken: page.NextPageToken,
		})
	}
}

func parseEventsQuery(r *http.Request, now time.Time) (calendar.Query, string, error) {
	qs := r.URL.Query()
	q := calendar.Query{MaxResults: defaultMaxResults, PageToken: qs.Get("page_token")}

	timeMin := now
	if raw := qs.Get("time_min"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return q, "time_min must be RFC3339", err
		}
		timeMin = t
	}
	timeMax := timeMin.Add(defaultWindow)
	if raw := qs.Get("time_max"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return q, "time_max must be RFC3339", err
		}
		timeMax = t
	}
	if !timeMax.After(timeMin) {
		return q, "time_max must be after time_min", errors.New("invalid range")
	}
	if timeMax.Sub(timeMin) > maxWindow {
		return q, "range must be at most 366 days", errors.New("range too large")
	}

	if raw := qs.Get("max_results"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxMaxResults {
			return q, "max_results must be an integer between 1 and 250", errors.New("bad max_results")
		}
		q.MaxResults = n
	}

	q.TimeMin = timeMin
	q.TimeMax = timeMax
	return q, "", nil
}

func mapCalendarError(err error) (status int, code string, retryAfter time.Duration) {
	var rate *calendar.RateLimitedError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not_found", 0
	case errors.Is(err, oauth.ErrAccountDisconnected):
		return http.StatusConflict, "account_disconnected", 0
	case errors.Is(err, oauth.ErrAccountRevoked):
		return http.StatusConflict, "account_revoked", 0
	case errors.Is(err, calendar.ErrCalendarUnavailable):
		return http.StatusConflict, "calendar_unavailable", 0
	case errors.As(err, &rate):
		return http.StatusTooManyRequests, "provider_rate_limited", rate.RetryAfter
	case errors.Is(err, calendar.ErrCalendarNotFound):
		return http.StatusNotFound, "not_found", 0
	case errors.Is(err, oauth.ErrProviderUnavailable), errors.Is(err, calendar.ErrProviderUnavailable):
		return http.StatusBadGateway, "provider_unavailable", 0
	case errors.Is(err, calendar.ErrProviderBadResponse):
		return http.StatusBadGateway, "provider_error", 0
	default:
		return http.StatusInternalServerError, "internal error", 0
	}
}
