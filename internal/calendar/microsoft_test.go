package calendar

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// graphFake serves /v1.0/me/calendarView with a fixed status/body and
// records the last request.
func graphFake(t *testing.T, status int, body string, headers map[string]string) (*MicrosoftSource, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r.Clone(context.Background())
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &MicrosoftSource{BaseURL: srv.URL, HTTP: srv.Client()}, &last
}

var graphQuery = Query{
	TimeMin:    time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
	TimeMax:    time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC),
	MaxResults: 25,
}

const graphPage = `{
  "@odata.nextLink": "https://graph.microsoft.com/v1.0/me/calendarView?startDateTime=2026-10-20T00:00:00Z&endDateTime=2026-10-27T00:00:00Z&$top=25&$skiptoken=SKIP-abc_123",
  "value": [
    {"id":"timed","subject":"Standup","isAllDay":false,"isCancelled":false,
     "start":{"dateTime":"2026-10-20T14:00:00.0000000","timeZone":"UTC"},
     "end":{"dateTime":"2026-10-20T14:30:00.0000000","timeZone":"UTC"},
     "originalStartTimeZone":"SA Pacific Standard Time",
     "location":{"displayName":"Teams"},
     "organizer":{"emailAddress":{"name":"Ana","address":"ana@contoso.com"}}},
    {"id":"allday","subject":"Holiday","isAllDay":true,"isCancelled":false,
     "start":{"dateTime":"2026-10-21T00:00:00.0000000","timeZone":"UTC"},
     "end":{"dateTime":"2026-10-22T00:00:00.0000000","timeZone":"UTC"},
     "originalStartTimeZone":"Romance Standard Time","location":{"displayName":""}},
    {"id":"cancelled","subject":"Gone","isAllDay":false,"isCancelled":true,
     "start":{"dateTime":"2026-10-22T10:00:00.0000000","timeZone":"UTC"},
     "end":{"dateTime":"2026-10-22T11:00:00.0000000","timeZone":"UTC"}},
    {"id":"bare","subject":"","isAllDay":false,"isCancelled":false,
     "start":{"dateTime":"2026-10-23T09:00:00","timeZone":"UTC"},
     "end":{"dateTime":"2026-10-23T10:00:00","timeZone":"UTC"},
     "originalStartTimeZone":"tzone://Microsoft/Custom"}
  ]
}`

func TestMicrosoftListEventsNormalizes(t *testing.T) {
	src, req := graphFake(t, http.StatusOK, graphPage, nil)

	page, err := src.ListEvents(context.Background(), "graph-access", graphQuery)
	if err != nil {
		t.Fatal(err)
	}

	// Request: fixed path, bearer, UTC preference, OData params with a literal $.
	if req.URL.Path != "/v1.0/me/calendarView" {
		t.Fatalf("path = %q", req.URL.Path)
	}
	if req.Header.Get("Authorization") != "Bearer graph-access" || req.Header.Get("Prefer") != `outlook.timezone="UTC"` {
		t.Fatalf("headers = %v", req.Header)
	}
	if strings.Contains(req.URL.RawQuery, "%24") {
		t.Fatalf("OData params must use a literal $: %s", req.URL.RawQuery)
	}
	q := req.URL.Query()
	for k, v := range map[string]string{
		"startDateTime": "2026-10-20T00:00:00Z", "endDateTime": "2026-10-27T00:00:00Z",
		"$top": "25", "$orderby": "start/dateTime", "$select": graphEventsSelect,
	} {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}

	want := []Event{
		{ID: "timed", Title: "Standup", Start: "2026-10-20T14:00:00Z", End: "2026-10-20T14:30:00Z",
			Timezone: "America/Bogota", Location: "Teams", Organizer: &Organizer{Email: "ana@contoso.com", Name: "Ana"}, Provider: "microsoft"},
		{ID: "allday", Title: "Holiday", Start: "2026-10-21", End: "2026-10-22", AllDay: true, Provider: "microsoft"},
		{ID: "bare", Title: "", Start: "2026-10-23T09:00:00Z", End: "2026-10-23T10:00:00Z", Provider: "microsoft"},
	}
	if len(page.Events) != len(want) {
		t.Fatalf("got %d events (cancelled must be dropped): %+v", len(page.Events), page.Events)
	}
	for i, w := range want {
		got := page.Events[i]
		if got.Organizer != nil && w.Organizer != nil && *got.Organizer == *w.Organizer {
			got.Organizer, w.Organizer = nil, nil
		}
		if got != w {
			t.Errorf("event %d = %+v, want %+v", i, got, w)
		}
	}
	if page.NextPageToken != "skiptoken:SKIP-abc_123" {
		t.Fatalf("next token = %q", page.NextPageToken)
	}
}

func TestMicrosoftListEventsHonorsNonUTCTimeZone(t *testing.T) {
	body := `{"value":[{"id":"x","subject":"s","start":{"dateTime":"2026-10-20T09:00:00.0000000","timeZone":"Pacific Standard Time"},
	  "end":{"dateTime":"2026-10-20T10:00:00.0000000","timeZone":"Pacific Standard Time"}}]}`
	src, _ := graphFake(t, http.StatusOK, body, nil)

	page, err := src.ListEvents(context.Background(), "a", graphQuery)
	if err != nil {
		t.Fatal(err)
	}
	// 09:00 in Los Angeles (PDT, UTC-7 on Oct 20) is 16:00 UTC.
	if page.Events[0].Start != "2026-10-20T16:00:00Z" {
		t.Fatalf("start = %q", page.Events[0].Start)
	}
}

func TestMicrosoftPagination(t *testing.T) {
	tests := []struct {
		name, nextLink, wantToken string
		wantErr                   error
	}{
		{"skiptoken", "https://graph.microsoft.com/v1.0/me/calendarView?$skiptoken=abc", "skiptoken:abc", nil},
		{"skip", "https://graph.microsoft.com/v1.0/me/calendarView?$skip=50", "skip:50", nil},
		// Only the paging value is kept, whatever host the link names.
		{"foreign host", "https://evil.example/steal?$skiptoken=abc", "skiptoken:abc", nil},
		{"no paging value", "https://graph.microsoft.com/v1.0/me/calendarView?$top=10", "", ErrProviderBadResponse},
		{"non-numeric skip", "https://graph.microsoft.com/v1.0/me/calendarView?$skip=ten", "", ErrProviderBadResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, _ := graphFake(t, http.StatusOK, `{"value":[],"@odata.nextLink":"`+tt.nextLink+`"}`, nil)
			page, err := src.ListEvents(context.Background(), "a", graphQuery)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil || page.NextPageToken != tt.wantToken {
				t.Fatalf("token = %q err = %v, want %q", page.NextPageToken, err, tt.wantToken)
			}
		})
	}
}

// A page token sends only the paging value, to Relay's fixed Graph base URL.
func TestMicrosoftPageTokenRequest(t *testing.T) {
	src, req := graphFake(t, http.StatusOK, `{"value":[]}`, nil)

	q := graphQuery
	q.PageToken = "skiptoken:abc"
	if _, err := src.ListEvents(context.Background(), "a", q); err != nil {
		t.Fatal(err)
	}
	if req.URL.Query().Get("$skiptoken") != "abc" || req.URL.Path != "/v1.0/me/calendarView" {
		t.Fatalf("request = %s", req.URL)
	}

	for _, bad := range []string{"abc", "skip:-1", "skip:x", "skiptoken:", "url:https://evil.example"} {
		q.PageToken = bad
		if _, err := src.ListEvents(context.Background(), "a", q); !errors.Is(err, ErrInvalidPageToken) {
			t.Errorf("page token %q: err = %v, want ErrInvalidPageToken", bad, err)
		}
	}
}

func TestMicrosoftListEventsErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		headers   map[string]string
		wantErr   error
		wantRetry time.Duration
	}{
		{"token rejected", 401, `{}`, nil, ErrTokenRejected, 0},
		{"throttled 429", 429, `{}`, map[string]string{"Retry-After": "7"}, nil, 7 * time.Second},
		{"throttled 503 with Retry-After", 503, `{}`, map[string]string{"Retry-After": "3"}, nil, 3 * time.Second},
		{"graph down", 503, `{}`, nil, ErrProviderUnavailable, 0},
		{"server error", 500, `{}`, nil, ErrProviderUnavailable, 0},
		{"no mailbox", 404, `{"error":{"code":"MailboxNotEnabledForRESTAPI","message":"..."}}`, nil, ErrCalendarUnavailable, 0},
		{"no mailbox (403)", 403, `{"error":{"code":"MailboxNotEnabledForRESTAPI"}}`, nil, ErrCalendarUnavailable, 0},
		{"not found", 404, `{"error":{"code":"ErrorItemNotFound"}}`, nil, ErrCalendarNotFound, 0},
		{"bad request", 400, `{}`, nil, ErrProviderBadResponse, 0},
		{"not json", 200, `<html>`, nil, ErrProviderBadResponse, 0},
		{"unknown event zone", 200, `{"value":[{"id":"x","start":{"dateTime":"2026-10-20T09:00:00","timeZone":"Mars Standard Time"},"end":{"dateTime":"2026-10-20T10:00:00","timeZone":"UTC"}}]}`, nil, ErrProviderBadResponse, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, _ := graphFake(t, tt.status, tt.body, tt.headers)
			_, err := src.ListEvents(context.Background(), "a", graphQuery)
			if tt.wantRetry > 0 {
				var rl *RateLimitedError
				if !errors.As(err, &rl) || rl.RetryAfter != tt.wantRetry {
					t.Fatalf("err = %v, want RateLimitedError{%v}", err, tt.wantRetry)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestMicrosoftListEventsBodyTooLarge(t *testing.T) {
	src, _ := graphFake(t, http.StatusOK, `{"value":[`+strings.Repeat(" ", maxGraphBody)+`]}`, nil)
	if _, err := src.ListEvents(context.Background(), "a", graphQuery); !errors.Is(err, ErrProviderBadResponse) {
		t.Fatalf("err = %v, want ErrProviderBadResponse", err)
	}
}

func TestIANAZone(t *testing.T) {
	for in, want := range map[string]string{
		"SA Pacific Standard Time": "America/Bogota",
		"Pacific Standard Time":    "America/Los_Angeles",
		"Romance Standard Time":    "Europe/Paris",
		"UTC":                      "UTC",
		"tzone://Microsoft/Utc":    "UTC",
		"Europe/Madrid":            "Europe/Madrid", // Graph sometimes returns IANA
		"tzone://Microsoft/Custom": "",
		"":                         "",
		"Mars Standard Time":       "",
		"../../etc/passwd":         "",
	} {
		if got := ianaZone(in); got != want {
			t.Errorf("ianaZone(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every generated mapping must be loadable, or normalization would fail at
// runtime for that zone.
func TestWindowsZonesAreLoadable(t *testing.T) {
	if len(windowsZones) < 130 {
		t.Fatalf("only %d zones: regenerate with go generate ./internal/calendar", len(windowsZones))
	}
	for win, iana := range windowsZones {
		if _, err := time.LoadLocation(iana); err != nil {
			t.Errorf("%q → %q: %v", win, iana, err)
		}
	}
}
