package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

type fakeCalendar struct {
	page  calendar.Page
	err   error
	lastQ calendar.Query
	calls int
}

func (f *fakeCalendar) ListEvents(_ context.Context, _, _ string, q calendar.Query) (calendar.Page, error) {
	f.calls++
	f.lastQ = q
	return f.page, f.err
}

func calendarsRouter(t *testing.T, cal CalendarReader, key apikey.Key) http.Handler {
	t.Helper()
	return NewRouter(Deps{
		Log:      discardLogger(),
		Tenants:  &fakeLookup{tenants: map[string]store.Tenant{key.Hash: {ID: "tenant-1", Name: "T", KeyPrefix: key.KeyPrefix}}},
		Env:      apikey.EnvTest,
		Calendar: cal,
	})
}

func TestListEventsRequiresAPIKey(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeCalendar{page: calendar.Page{Events: []calendar.Event{}}}
	router := calendarsRouter(t, cal, key)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/calendars/acct/events", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if cal.calls != 0 {
		t.Fatal("must not call calendar without key")
	}
}

func TestListEventsQueryDefaultsAndValidation(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeCalendar{page: calendar.Page{Events: []calendar.Event{}}}
	router := calendarsRouter(t, cal, key)

	req := httptest.NewRequest(http.MethodGet, "/v1/calendars/acct/events", nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	rec := httptest.NewRecorder()
	before := time.Now().UTC()
	router.ServeHTTP(rec, req)
	after := time.Now().UTC()
	if rec.Code != 200 {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if cal.lastQ.MaxResults != 50 {
		t.Fatalf("max = %d", cal.lastQ.MaxResults)
	}
	if cal.lastQ.TimeMin.Before(before.Add(-time.Second)) || cal.lastQ.TimeMin.After(after.Add(time.Second)) {
		t.Fatalf("time_min default = %v", cal.lastQ.TimeMin)
	}
	wantMax := cal.lastQ.TimeMin.Add(30 * 24 * time.Hour)
	if !cal.lastQ.TimeMax.Equal(wantMax) {
		t.Fatalf("time_max = %v want %v", cal.lastQ.TimeMax, wantMax)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("cache = %q", cc)
	}

	tests := []struct {
		query string
		want  string
	}{
		{"max_results=0", "invalid_query"},
		{"max_results=999", "invalid_query"},
		{"time_min=not-rfc", "invalid_query"},
		{"time_min=2026-01-01T00:00:00Z&time_max=2025-01-01T00:00:00Z", "invalid_query"},
		{"time_min=2020-01-01T00:00:00Z&time_max=2022-01-02T00:00:00Z", "invalid_query"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/calendars/acct/events?"+tt.query, nil)
		req.Header.Set("Authorization", "Bearer "+key.Plaintext)
		router.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("%s: status %d", tt.query, rec.Code)
		}
		var body queryErrorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Error != tt.want || body.Detail == "" {
			t.Fatalf("%s: body %+v", tt.query, body)
		}
	}
}

func TestListEventsEmptyArrayNotNull(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeCalendar{page: calendar.Page{Events: nil}}
	router := calendarsRouter(t, cal, key)
	req := httptest.NewRequest(http.MethodGet, "/v1/calendars/acct/events", nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestListEventsErrorTable(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantError  string
		retry      string
	}{
		{"not found", store.ErrNotFound, 404, "not_found", ""},
		{"revoked", oauth.ErrAccountRevoked, 409, "account_revoked", ""},
		{"rate", &calendar.RateLimitedError{RetryAfter: 7 * time.Second}, 429, "provider_rate_limited", "7"},
		{"unavailable", calendar.ErrProviderUnavailable, 502, "provider_unavailable", ""},
		{"bad response", calendar.ErrProviderBadResponse, 502, "provider_error", ""},
		{"internal", errors.New("boom"), 500, "internal error", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cal := &fakeCalendar{err: tt.err}
			router := calendarsRouter(t, cal, key)
			req := httptest.NewRequest(http.MethodGet, "/v1/calendars/acct/events", nil)
			req.Header.Set("Authorization", "Bearer "+key.Plaintext)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
			}
			var body errorBody
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body.Error != tt.wantError {
				t.Fatalf("error = %q", body.Error)
			}
			if tt.retry != "" && rec.Header().Get("Retry-After") != tt.retry {
				t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
			}
		})
	}
}
