package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/httpapi/middleware"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/ratelimit"
	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/testutil/contract"
)

// contractRouter is NewRouter with every response checked against the spec.
func contractRouter(t testing.TB, d Deps) http.Handler {
	t.Helper()
	spec, err := contract.Default()
	if err != nil {
		t.Fatal(err)
	}
	return spec.Wrap(t, NewRouter(d))
}

type noWebhooks struct{}

func (noWebhooks) Google(http.ResponseWriter, *http.Request)             {}
func (noWebhooks) Microsoft(http.ResponseWriter, *http.Request)          {}
func (noWebhooks) MicrosoftLifecycle(http.ResponseWriter, *http.Request) {}

type noCalendar struct{ fakeWrites }

func (noCalendar) ListEvents(context.Context, string, string, calendar.Query) (calendar.Page, error) {
	return calendar.Page{}, nil
}

// With every optional part on, the router and the spec must list the same
// routes: a new handler needs its docs, and the docs must not promise a route.
func TestRoutesMatchTheSpec(t *testing.T) {
	spec, err := contract.Default()
	if err != nil {
		t.Fatal(err)
	}
	limit := ratelimit.Limit{PerMinute: 60, Burst: 5}
	_, served := newRouter(Deps{
		Log: discardLogger(), Tenants: &fakeLookup{}, Env: apikey.EnvTest,
		Calendar: &noCalendar{}, CalendarWrite: &noCalendar{}, Idempotency: &memoryKeys{},
		Webhooks:  noWebhooks{},
		RateLimit: middleware.NewRateLimiter(discardLogger(), ratelimit.NewLocal(), limit, limit),
	})
	documented := spec.Routes()

	for _, route := range served {
		if !slices.Contains(documented, route) {
			t.Errorf("%s is served but not in api/openapi.yaml", route)
		}
	}
	for _, route := range documented {
		if !slices.Contains(served, route) {
			t.Errorf("%s is in api/openapi.yaml but not served", route)
		}
	}
	if len(served) == 0 {
		t.Fatal("no routes recorded")
	}
}

// bearerRoutes is one request per authenticated route.
var bearerRoutes = []struct{ method, path, body string }{
	{http.MethodGet, "/v1/me", ""},
	{http.MethodGet, "/v1/connect/google", ""},
	{http.MethodGet, "/v1/accounts", ""},
	{http.MethodDelete, "/v1/accounts/a1", ""},
	{http.MethodGet, "/v1/calendars/a1/events", ""},
	{http.MethodPost, "/v1/calendars/a1/events", eventBody},
	{http.MethodPatch, "/v1/calendars/a1/events/e1", `{"title":"x"}`},
	{http.MethodDelete, "/v1/calendars/a1/events/e1", ""},
}

func fullRouter(t *testing.T, key apikey.Key, cal *noCalendar, rl *middleware.RateLimiter) http.Handler {
	t.Helper()
	deps := Deps{
		Log:     discardLogger(),
		Tenants: &fakeLookup{tenants: map[string]store.Tenant{key.Hash: {ID: "tenant-1", Name: "T", KeyPrefix: key.KeyPrefix}}},
		Env:     apikey.EnvTest, Calendar: cal, CalendarWrite: cal, Idempotency: &memoryKeys{},
		Connect: fakeConnect{}, Accounts: &fakeAccountLister{}, Disconnect: &fakeDisconnector{}, RateLimit: rl,
	}
	return contractRouter(t, deps)
}

type fakeConnect struct{}

func (fakeConnect) Start(context.Context, string, oauth.Provider, ...oauth.Feature) (string, error) {
	return "https://example.com/consent", nil
}

// The same promise on every route: no key is 401, a spent budget is 429.
func TestEveryRouteAnswersAuthAndRateLimit(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	for _, route := range bearerRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			open := fullRouter(t, key, &noCalendar{}, nil)
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			rec := httptest.NewRecorder()
			open.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("no key: status = %d", rec.Code)
			}

			spent := ratelimit.Limit{PerMinute: 1, Burst: 1}
			limited := fullRouter(t, key, &noCalendar{}, middleware.NewRateLimiter(discardLogger(), ratelimit.NewLocal(), spent, spent))
			send(limited, route.method, route.path, route.body, key, nil)
			if rec := send(limited, route.method, route.path, route.body, key, nil); rec.Code != http.StatusTooManyRequests {
				t.Fatalf("spent budget: status = %d", rec.Code)
			}
		})
	}
}

// Each write route, each failure the service can return.
func TestEveryWriteRouteAnswersItsErrors(t *testing.T) {
	failures := []struct {
		name   string
		err    error
		status int
	}{
		{"scope", &calendar.InsufficientScopeError{Provider: "google", Required: []string{oauth.GoogleCalendarEvents}}, http.StatusForbidden},
		{"not found", store.ErrNotFound, http.StatusNotFound},
		{"revoked", oauth.ErrAccountRevoked, http.StatusConflict},
		{"throttled", &calendar.RateLimitedError{}, http.StatusTooManyRequests},
		{"invalid", &calendar.ValidationError{Code: calendar.CodeInvalidEvent, Detail: "no"}, http.StatusBadRequest},
		{"provider down", calendar.ErrProviderUnavailable, http.StatusBadGateway},
		{"unexpected", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, route := range bearerRoutes[5:] {
		for _, f := range failures {
			t.Run(route.method+" "+f.name, func(t *testing.T) {
				key := apikey.Generate(apikey.EnvTest)
				cal := &noCalendar{}
				cal.err = f.err
				rec := send(fullRouter(t, key, cal, nil), route.method, route.path, route.body, key, nil)
				if rec.Code != f.status {
					t.Fatalf("status = %d, want %d: %s", rec.Code, f.status, rec.Body.String())
				}
			})
		}
	}
}

func TestEveryWriteRouteAnswersIdempotency(t *testing.T) {
	for _, route := range bearerRoutes[5:] {
		t.Run(route.method, func(t *testing.T) {
			key := apikey.Generate(apikey.EnvTest)
			hdr := map[string]string{"Idempotency-Key": "k1"}

			keys := &memoryKeys{}
			h := writesRouter(t, &fakeWrites{}, keys, key)
			if rec := send(h, route.method, route.path, route.body, key, hdr); rec.Code >= 300 {
				t.Fatalf("first: %d", rec.Code)
			}
			if rec := send(h, route.method, route.path, route.body, key, hdr); rec.Code >= 300 {
				t.Fatalf("replay: %d", rec.Code)
			}
			if rec := send(h, route.method, strings.Replace(route.path, "a1", "a2", 1), route.body, key, hdr); rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("reused key: %d", rec.Code)
			}

			req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			running := &memoryKeys{
				rows:   map[string]*store.StoredResponse{"k1": nil},
				hashes: map[string]string{"k1": requestHash(req, []byte(route.body))},
			}
			rec := send(writesRouter(t, &fakeWrites{}, running, key), route.method, route.path, route.body, key, hdr)
			if rec.Code != http.StatusConflict {
				t.Fatalf("in progress: %d", rec.Code)
			}
		})
	}
}

func TestPatchBodyTooLarge(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	rec := send(writesRouter(t, &fakeWrites{}, nil, key), http.MethodPatch, "/v1/calendars/a1/events/e1", strings.Repeat("x", maxEventBody+1), key, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAccountsListFailureIsDocumented(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	rec := doAuthed(accountsRouter(t, key, &fakeAccountLister{err: errors.New("db down")}, &fakeDisconnector{}), http.MethodGet, "/v1/accounts", key)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestOptionalRoutesAreOffWithoutTheirDeps(t *testing.T) {
	_, served := newRouter(Deps{Log: discardLogger(), Tenants: &fakeLookup{}, Env: apikey.EnvTest})
	for _, route := range []string{"POST /v1/webhooks/google", "POST /v1/calendars/{id}/events"} {
		if slices.Contains(served, route) {
			t.Errorf("%s must be off", route)
		}
	}
}
