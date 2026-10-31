package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/testutil/fakegoogle"
	"github.com/sebasBetancourt/relay/internal/testutil/fakemicrosoft"
)

const (
	writeFeature = "?features=calendar.write"
	timedEvent   = `{"title":"Planning","location":"Room 1","start":"2026-11-02T09:00:00-05:00","end":"2026-11-02T10:00:00-05:00","timezone":"America/Bogota"}`
	window       = "time_min=2026-11-01T00:00:00Z&time_max=2026-11-10T00:00:00Z"
)

func (f *flow) send(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	return sendOn(f.router, f.authHdr(), method, path, body, hdr)
}

func sendOn(router http.Handler, auth map[string]string, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range auth {
		req.Header.Set(k, v)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// connectWrite connects an account with the write scope.
func (f *flow) connectWrite(provider string) string {
	f.t.Helper()
	rec := f.do(http.MethodGet, "/v1/connect/"+provider+writeFeature, f.authHdr())
	if rec.Code != http.StatusFound {
		f.t.Fatalf("connect %s: status %d body %s", provider, rec.Code, rec.Body.String())
	}
	cb := f.callback(f.fake(provider).Consent(rec.Header().Get("Location")))
	if cb.Code != http.StatusOK {
		f.t.Fatalf("callback %s: %d %s", provider, cb.Code, cb.Body.String())
	}
	var body connectedBody
	if err := json.Unmarshal(cb.Body.Bytes(), &body); err != nil {
		f.t.Fatal(err)
	}
	return body.AccountID
}

func (f *flow) writeCount(provider string) int {
	if provider == "microsoft" {
		return len(f.ms.Writes())
	}
	return len(f.google.Writes())
}

func created(t *testing.T, rec *httptest.ResponseRecorder) calendar.Event {
	t.Helper()
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
	}
	var ev calendar.Event
	if err := json.Unmarshal(rec.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func (f *flow) findEvent(accountID, id string) (calendar.Event, bool) {
	f.t.Helper()
	rec := f.events(accountID, "?"+window)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	var page eventsPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		f.t.Fatal(err)
	}
	for _, e := range page.Events {
		if e.ID == id {
			return e, true
		}
	}
	return calendar.Event{}, false
}

// Create, read back, edit, refuse a stale edit, delete: same calls, same JSON.
func TestEventLifecycle(t *testing.T) {
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			f := newFlow(t)
			id := f.connectWrite(provider)
			path := "/v1/calendars/" + id + "/events"

			ev := created(t, f.send(http.MethodPost, path, timedEvent, nil))
			if ev.Title != "Planning" || ev.Location != "Room 1" || ev.Provider != provider || ev.Version == "" {
				t.Fatalf("created = %+v", ev)
			}
			wantStart := time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)
			got, err := time.Parse(time.RFC3339, ev.Start)
			if err != nil || !got.Equal(wantStart) {
				t.Fatalf("start = %q, want %s", ev.Start, wantStart)
			}
			if ev.Timezone != "America/Bogota" {
				t.Fatalf("timezone = %q", ev.Timezone)
			}

			listed, ok := f.findEvent(id, ev.ID)
			if !ok || listed.Title != "Planning" || listed.Version != ev.Version {
				t.Fatalf("listed = %+v (found %v)", listed, ok)
			}

			edited := created(t, f.send(http.MethodPatch, path+"/"+ev.ID, `{"title":"Renamed"}`, map[string]string{"If-Match": ev.Version}))
			if edited.Title != "Renamed" || edited.Version == ev.Version {
				t.Fatalf("edited = %+v", edited)
			}

			rec := f.send(http.MethodPatch, path+"/"+ev.ID, `{"title":"Late"}`, map[string]string{"If-Match": ev.Version})
			if rec.Code != http.StatusPreconditionFailed || !strings.Contains(rec.Body.String(), "version_conflict") {
				t.Fatalf("stale patch: %d %s", rec.Code, rec.Body.String())
			}

			if rec := f.send(http.MethodDelete, path+"/"+ev.ID, "", nil); rec.Code != http.StatusNoContent {
				t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
			}
			if _, ok := f.findEvent(id, ev.ID); ok {
				t.Fatal("the event must be gone")
			}
			if rec := f.send(http.MethodDelete, path+"/"+ev.ID, "", nil); rec.Code != http.StatusNotFound {
				t.Fatalf("second delete: %d", rec.Code)
			}
		})
	}
}

func TestAllDayEventIsADateWithExclusiveEnd(t *testing.T) {
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			f := newFlow(t)
			id := f.connectWrite(provider)
			ev := created(t, f.send(http.MethodPost, "/v1/calendars/"+id+"/events",
				`{"title":"Off","all_day":true,"start":"2026-11-02","end":"2026-11-03"}`, nil))
			if !ev.AllDay || ev.Start != "2026-11-02" || ev.End != "2026-11-03" {
				t.Fatalf("event = %+v", ev)
			}
		})
	}
}

// The same request reaches each provider in its own words.
func TestRecurrenceReachesEachProvider(t *testing.T) {
	const body = `{"title":"Standup","start":"2026-11-02T09:00:00-05:00","end":"2026-11-02T09:30:00-05:00",
		"timezone":"America/Bogota","recurrence":{"freq":"WEEKLY","by_day":["MO","WE"],"count":10}}`

	t.Run("google", func(t *testing.T) {
		f := newFlow(t)
		id := f.connectWrite("google")
		created(t, f.send(http.MethodPost, "/v1/calendars/"+id+"/events", body, nil))
		w := f.google.Writes()[0]
		rules := w.Body["recurrence"].([]any)
		if len(rules) != 1 || rules[0] != "RRULE:FREQ=WEEKLY;BYDAY=MO,WE;COUNT=10" {
			t.Fatalf("recurrence = %v", rules)
		}
		if tz := w.Body["start"].(map[string]any)["timeZone"]; tz != "America/Bogota" {
			t.Fatalf("timeZone = %v", tz)
		}
		if w.Query.Get("sendUpdates") != "all" {
			t.Fatalf("sendUpdates = %q", w.Query.Get("sendUpdates"))
		}
	})

	t.Run("microsoft", func(t *testing.T) {
		f := newFlow(t)
		id := f.connectWrite("microsoft")
		created(t, f.send(http.MethodPost, "/v1/calendars/"+id+"/events", body, nil))
		w := f.ms.Writes()[0]
		rec := w.Body["recurrence"].(map[string]any)
		pattern, rng := rec["pattern"].(map[string]any), rec["range"].(map[string]any)
		if pattern["type"] != "weekly" || fmt.Sprint(pattern["daysOfWeek"]) != "[monday wednesday]" {
			t.Fatalf("pattern = %v", pattern)
		}
		if rng["type"] != "numbered" || rng["numberOfOccurrences"] != float64(10) || rng["startDate"] != "2026-11-02" {
			t.Fatalf("range = %v", rng)
		}
		start := w.Body["start"].(map[string]any)
		if start["dateTime"] != "2026-11-02T09:00:00" || start["timeZone"] != "SA Pacific Standard Time" {
			t.Fatalf("start = %v", start)
		}
	})
}

func TestUnsupportedRecurrenceCreatesNothing(t *testing.T) {
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			f := newFlow(t)
			id := f.connectWrite(provider)
			rec := f.send(http.MethodPost, "/v1/calendars/"+id+"/events",
				`{"start":"2026-10-31T09:00:00-05:00","end":"2026-10-31T10:00:00-05:00","timezone":"America/Bogota","recurrence":{"freq":"MONTHLY"}}`, nil)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_recurrence") {
				t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
			}
			if n := f.writeCount(provider); n != 0 {
				t.Fatalf("writes = %d", n)
			}
		})
	}
}

func TestIdempotentCreate(t *testing.T) {
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			f := newFlow(t)
			id := f.connectWrite(provider)
			path := "/v1/calendars/" + id + "/events"
			hdr := map[string]string{"Idempotency-Key": "retry-1"}

			first := created(t, f.send(http.MethodPost, path, timedEvent, hdr))
			again := created(t, f.send(http.MethodPost, path, timedEvent, hdr))
			if first.ID != again.ID {
				t.Fatalf("ids = %q, %q", first.ID, again.ID)
			}
			if n := f.writeCount(provider); n != 1 {
				t.Fatalf("writes = %d, want one event", n)
			}
			rec := f.send(http.MethodPost, path, strings.Replace(timedEvent, "Planning", "Other", 1), hdr)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("other body: %d", rec.Code)
			}
		})
	}
}

// Two replicas, one key, at the same time: a single event at the provider.
func TestIdempotentCreateConcurrentAcrossReplicas(t *testing.T) {
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			f := newFlow(t)
			id := f.connectWrite(provider)
			routers := []http.Handler{f.router, f.replica()}
			hdr := map[string]string{"Idempotency-Key": "race"}

			var wg gosync.WaitGroup
			codes := make(chan int, 10)
			for i := range 10 {
				wg.Add(1)
				go func(r http.Handler) {
					defer wg.Done()
					codes <- sendOn(r, f.authHdr(), http.MethodPost, "/v1/calendars/"+id+"/events", timedEvent, hdr).Code
				}(routers[i%2])
			}
			wg.Wait()
			close(codes)
			for c := range codes {
				if c != http.StatusCreated && c != http.StatusConflict {
					t.Fatalf("code = %d", c)
				}
			}
			if n := f.writeCount(provider); n != 1 {
				t.Fatalf("writes = %d, want exactly one event", n)
			}
		})
	}
}

func TestWriteNeedsConsent(t *testing.T) {
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			f := newFlow(t)
			id := f.connectThrough(provider) // read-only

			rec := f.send(http.MethodPost, "/v1/calendars/"+id+"/events", timedEvent, nil)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Error          string   `json:"error"`
				RequiredScopes []string `json:"required_scopes"`
				ReconnectURL   string   `json:"reconnect_url"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error != "insufficient_scope" || len(body.RequiredScopes) != 1 {
				t.Fatalf("body = %+v", body)
			}

			// The answer's own URL fixes it, on the same account.
			res := f.do(http.MethodGet, body.ReconnectURL, f.authHdr())
			if res.Code != http.StatusFound {
				t.Fatalf("reconnect: %d", res.Code)
			}
			if cb := f.callback(f.fake(provider).Consent(res.Header().Get("Location"))); cb.Code != http.StatusOK {
				t.Fatalf("callback %d", cb.Code)
			}
			created(t, f.send(http.MethodPost, "/v1/calendars/"+id+"/events", timedEvent, nil))
		})
	}
}

func TestConnectAsksForWriteOnlyOnRequest(t *testing.T) {
	f := newFlow(t)
	scope := func(loc string) string {
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatal(err)
		}
		return u.Query().Get("scope")
	}
	if got := scope(f.connect("google")); strings.Contains(got, fakegoogle.ScopeCalendarEvents) {
		t.Fatalf("plain connect must stay read-only: %q", got)
	}
	rec := f.do(http.MethodGet, "/v1/connect/google"+writeFeature, f.authHdr())
	loc := rec.Header().Get("Location")
	if !strings.Contains(scope(loc), fakegoogle.ScopeCalendarEvents) || !strings.Contains(loc, "include_granted_scopes=true") {
		t.Fatalf("location = %s", loc)
	}
	if rec := f.do(http.MethodGet, "/v1/connect/google?features=nope", f.authHdr()); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown feature: %d", rec.Code)
	}
}

func TestWriteRetriesAfterExpiredToken(t *testing.T) {
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			f := newFlow(t)
			id := f.connectWrite(provider)
			f.fake(provider).ExpireAccessTokens()
			before := f.fake(provider).Calls().Refresh
			created(t, f.send(http.MethodPost, "/v1/calendars/"+id+"/events", timedEvent, nil))
			if d := f.fake(provider).Calls().Refresh - before; d != 1 {
				t.Fatalf("refreshes = %d", d)
			}
			if n := f.writeCount(provider); n != 1 {
				t.Fatalf("writes = %d", n)
			}
		})
	}
}

func TestSingleOccurrenceIsRefused(t *testing.T) {
	start := time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)

	check := func(t *testing.T, f *flow, id string) {
		t.Helper()
		path := "/v1/calendars/" + id + "/events/occ-1"
		for _, rec := range []*httptest.ResponseRecorder{
			f.send(http.MethodPatch, path, `{"title":"x"}`, nil),
			f.send(http.MethodDelete, path, "", nil),
		} {
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_recurrence") {
				t.Fatalf("code = %d body %s", rec.Code, rec.Body.String())
			}
		}
	}

	t.Run("google", func(t *testing.T) {
		f := newFlow(t)
		id := f.connectWrite("google")
		f.google.SetEvents(fakegoogle.Event{
			ID: "occ-1", Summary: "One", Start: start.Format(time.RFC3339), End: start.Add(time.Hour).Format(time.RFC3339),
			TimeZone: "UTC", RecurringEventID: "series",
		})
		check(t, f, id)
	})
	t.Run("microsoft", func(t *testing.T) {
		f := newFlow(t)
		id := f.connectWrite("microsoft")
		f.ms.SetEvents(fakemicrosoft.Event{ID: "occ-1", Subject: "One", Start: start, End: start.Add(time.Hour), Type: "occurrence"})
		check(t, f, id)
	})
}

func TestWriteOnAnotherTenantsAccountIsNotFound(t *testing.T) {
	f := newFlow(t)
	id := f.connectWrite("google")
	other := f.otherTenantKey()
	rec := sendOn(f.router, other, http.MethodPost, "/v1/calendars/"+id+"/events", timedEvent, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}
	if n := f.writeCount("google"); n != 0 {
		t.Fatalf("writes = %d", n)
	}
}
