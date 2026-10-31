package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

type fakeWrites struct {
	err     error
	calls   int
	version string
	in      calendar.EventInput
}

func (f *fakeWrites) CreateEvent(_ context.Context, _, _ string, in calendar.EventInput) (calendar.Event, error) {
	f.calls++
	f.in = in
	return calendar.Event{ID: "e1", Title: in.Title, Provider: "google"}, f.err
}

func (f *fakeWrites) UpdateEvent(_ context.Context, _, _, _ string, _ calendar.EventPatch, version string) (calendar.Event, error) {
	f.calls++
	f.version = version
	return calendar.Event{ID: "e1", Provider: "google"}, f.err
}

func (f *fakeWrites) DeleteEvent(context.Context, string, string, string) error {
	f.calls++
	return f.err
}

// memoryKeys is the store's contract without Postgres.
type memoryKeys struct {
	rows      map[string]*store.StoredResponse // nil value: in progress
	hashes    map[string]string
	completed int
	released  int
}

func (m *memoryKeys) Begin(_ context.Context, _, key, hash string) (*store.StoredResponse, error) {
	if m.rows == nil {
		m.rows, m.hashes = map[string]*store.StoredResponse{}, map[string]string{}
	}
	stored, seen := m.rows[key]
	if !seen {
		m.rows[key], m.hashes[key] = nil, hash
		return nil, nil
	}
	if m.hashes[key] != hash {
		return nil, store.ErrIdempotencyMismatch
	}
	if stored == nil {
		return nil, store.ErrIdempotencyInProgress
	}
	return stored, nil
}

func (m *memoryKeys) Complete(_ context.Context, _, key string, res store.StoredResponse) error {
	m.completed++
	m.rows[key] = &res
	return nil
}

func (m *memoryKeys) Release(_ context.Context, _, key string) error {
	m.released++
	delete(m.rows, key)
	return nil
}

func writesRouter(t *testing.T, cal CalendarWriter, keys IdempotencyStore, key apikey.Key) http.Handler {
	t.Helper()
	return contractRouter(t, Deps{
		Log:           discardLogger(),
		Tenants:       &fakeLookup{tenants: map[string]store.Tenant{key.Hash: {ID: "tenant-1", Name: "T", KeyPrefix: key.KeyPrefix}}},
		Env:           apikey.EnvTest,
		CalendarWrite: cal,
		Idempotency:   keys,
	})
}

func send(h http.Handler, method, path, body string, key apikey.Key, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const eventBody = `{"title":"Planning","start":"2026-11-02T09:00:00-05:00","end":"2026-11-02T10:00:00-05:00","timezone":"America/Bogota"}`

func TestCreateEvent(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeWrites{}
	rec := send(writesRouter(t, cal, nil, key), http.MethodPost, "/v1/calendars/a1/events", eventBody, key, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
	}
	if cal.in.TimeZone != "America/Bogota" {
		t.Fatalf("input = %+v", cal.in)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestCreateEventRefusesUnknownFields(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeWrites{}
	rec := send(writesRouter(t, cal, nil, key), http.MethodPost, "/v1/calendars/a1/events", `{"title":"x","atendees":["a@b.co"]}`, key, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_body") {
		t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
	}
	if cal.calls != 0 {
		t.Fatal("a typo must not create an event without its guests")
	}
}

func TestCreateEventBodyTooLarge(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	rec := send(writesRouter(t, &fakeWrites{}, nil, key), http.MethodPost, "/v1/calendars/a1/events", strings.Repeat("x", maxEventBody+1), key, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestWriteErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"recurrence", &calendar.ValidationError{Code: calendar.CodeUnsupportedRecurrence, Detail: "no"}, http.StatusBadRequest, "unsupported_recurrence"},
		{"other tenant", store.ErrNotFound, http.StatusNotFound, "not_found"},
		{"revoked", oauth.ErrAccountRevoked, http.StatusConflict, "account_revoked"},
		{"provider down", calendar.ErrProviderUnavailable, http.StatusBadGateway, "provider_unavailable"},
		{"throttled", &calendar.RateLimitedError{}, http.StatusTooManyRequests, "provider_rate_limited"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := apikey.Generate(apikey.EnvTest)
			rec := send(writesRouter(t, &fakeWrites{err: tt.err}, nil, key), http.MethodPost, "/v1/calendars/a1/events", eventBody, key, nil)
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.code) {
				t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestInsufficientScopeTellsHowToFixIt(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeWrites{err: &calendar.InsufficientScopeError{Provider: "google", Required: []string{oauth.GoogleCalendarEvents}}}
	rec := send(writesRouter(t, cal, nil, key), http.MethodPost, "/v1/calendars/a1/events", eventBody, key, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d", rec.Code)
	}
	var body insufficientScopeBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "insufficient_scope" || body.RequiredScopes[0] != oauth.GoogleCalendarEvents ||
		body.ReconnectURL != "/v1/connect/google?features=calendar.write" {
		t.Fatalf("body = %+v", body)
	}
}

func TestStaleVersionIsRefused(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	h := writesRouter(t, &fakeWrites{err: calendar.ErrVersionConflict}, nil, key)
	rec := send(h, http.MethodPatch, "/v1/calendars/a1/events/e1", `{"title":"x"}`, key, map[string]string{"If-Match": "v1"})
	if rec.Code != http.StatusPreconditionFailed || !strings.Contains(rec.Body.String(), "version_conflict") {
		t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPatchPassesIfMatch(t *testing.T) {
	tests := []struct{ header, want string }{
		{"", ""},
		{"abc", "abc"},
		{`"abc"`, "abc"},
		{`W/"abc"`, "abc"},
		{"*", ""},
	}
	for _, tt := range tests {
		key := apikey.Generate(apikey.EnvTest)
		cal := &fakeWrites{}
		send(writesRouter(t, cal, nil, key), http.MethodPatch, "/v1/calendars/a1/events/e1", `{"title":"x"}`, key, map[string]string{"If-Match": tt.header})
		if cal.version != tt.want {
			t.Errorf("If-Match %q: version = %q, want %q", tt.header, cal.version, tt.want)
		}
	}
}

func TestDeleteEvent(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	rec := send(writesRouter(t, &fakeWrites{}, nil, key), http.MethodDelete, "/v1/calendars/a1/events/e1", "", key, nil)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("code = %d body %q", rec.Code, rec.Body.String())
	}
}

func TestIdempotencyReplaysTheFirstAnswer(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeWrites{}
	keys := &memoryKeys{}
	h := writesRouter(t, cal, keys, key)
	hdr := map[string]string{"Idempotency-Key": "k1"}

	first := send(h, http.MethodPost, "/v1/calendars/a1/events", eventBody, key, hdr)
	second := send(h, http.MethodPost, "/v1/calendars/a1/events", eventBody, key, hdr)

	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("codes = %d, %d", first.Code, second.Code)
	}
	if cal.calls != 1 {
		t.Fatalf("calls = %d, want 1", cal.calls)
	}
	if first.Body.String() != strings.TrimSuffix(second.Body.String(), "\n")+"\n" {
		t.Fatalf("bodies differ:\n%s\n%s", first.Body.String(), second.Body.String())
	}
	if first.Header().Get("Idempotent-Replayed") != "" || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("only the replay must be marked")
	}
}

func TestIdempotencyReplaysAnEmptyResponse(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeWrites{}
	h := writesRouter(t, cal, &memoryKeys{}, key)
	hdr := map[string]string{"Idempotency-Key": "k1"}
	send(h, http.MethodDelete, "/v1/calendars/a1/events/e1", "", key, hdr)
	rec := send(h, http.MethodDelete, "/v1/calendars/a1/events/e1", "", key, hdr)
	if rec.Code != http.StatusNoContent || cal.calls != 1 {
		t.Fatalf("code = %d calls = %d", rec.Code, cal.calls)
	}
}

func TestIdempotencyOtherBodyIsRefused(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeWrites{}
	h := writesRouter(t, cal, &memoryKeys{}, key)
	hdr := map[string]string{"Idempotency-Key": "k1"}
	send(h, http.MethodPost, "/v1/calendars/a1/events", eventBody, key, hdr)
	rec := send(h, http.MethodPost, "/v1/calendars/a1/events", `{"title":"Other"}`, key, hdr)
	if rec.Code != http.StatusUnprocessableEntity || cal.calls != 1 {
		t.Fatalf("code = %d calls = %d", rec.Code, cal.calls)
	}
}

// The same key on another route is another request, not a replay.
func TestIdempotencyCoversTheRoute(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	h := writesRouter(t, &fakeWrites{}, &memoryKeys{}, key)
	hdr := map[string]string{"Idempotency-Key": "k1"}
	send(h, http.MethodPost, "/v1/calendars/a1/events", eventBody, key, hdr)
	rec := send(h, http.MethodPost, "/v1/calendars/a2/events", eventBody, key, hdr)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestIdempotencyInProgressAsksToRetry(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	keys := &memoryKeys{rows: map[string]*store.StoredResponse{"k1": nil}, hashes: map[string]string{}}
	cal := &fakeWrites{}
	h := writesRouter(t, cal, keys, key)
	req := httptest.NewRequest(http.MethodPost, "/v1/calendars/a1/events", strings.NewReader(eventBody))
	keys.hashes["k1"] = requestHash(req, []byte(eventBody))
	rec := send(h, http.MethodPost, "/v1/calendars/a1/events", eventBody, key, map[string]string{"Idempotency-Key": "k1"})
	if rec.Code != http.StatusConflict || rec.Header().Get("Retry-After") == "" || cal.calls != 0 {
		t.Fatalf("code = %d calls = %d", rec.Code, cal.calls)
	}
}

// A failed write stored nothing, so the retry must reach the provider.
func TestIdempotencyFailureReleasesTheKey(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	cal := &fakeWrites{err: errors.Join(calendar.ErrProviderUnavailable)}
	keys := &memoryKeys{}
	h := writesRouter(t, cal, keys, key)
	hdr := map[string]string{"Idempotency-Key": "k1"}
	if rec := send(h, http.MethodPost, "/v1/calendars/a1/events", eventBody, key, hdr); rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d", rec.Code)
	}
	cal.err = nil
	if rec := send(h, http.MethodPost, "/v1/calendars/a1/events", eventBody, key, hdr); rec.Code != http.StatusCreated {
		t.Fatalf("retry code = %d", rec.Code)
	}
	if cal.calls != 2 || keys.released != 1 || keys.completed != 1 {
		t.Fatalf("calls %d released %d completed %d", cal.calls, keys.released, keys.completed)
	}
}

func TestIdempotencyKeyTooLong(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	rec := send(writesRouter(t, &fakeWrites{}, &memoryKeys{}, key), http.MethodPost, "/v1/calendars/a1/events", eventBody, key,
		map[string]string{"Idempotency-Key": strings.Repeat("k", maxIdempotencyKey+1)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
}
