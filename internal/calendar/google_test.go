package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGoogleListEventsHappyPath(t *testing.T) {
	var gotAuth, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotURL = r.URL.String()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"timeZone":      "America/Bogota",
			"nextPageToken": "page-2",
			"items": []map[string]any{
				{
					"id": "e1", "summary": "Standup", "location": "Zoom",
					"start":     map[string]string{"dateTime": "2026-10-20T09:00:00-05:00", "timeZone": "America/Bogota"},
					"end":       map[string]string{"dateTime": "2026-10-20T09:30:00-05:00", "timeZone": "America/Bogota"},
					"organizer": map[string]string{"email": "a@ex.com", "displayName": "Ada"},
				},
				{
					"id": "e2", "summary": "",
					"start": map[string]string{"dateTime": "2026-10-20T10:00:00Z"},
					"end":   map[string]string{"dateTime": "2026-10-20T11:00:00Z"},
				},
				{
					"id": "e3", "summary": "Holiday",
					"start": map[string]string{"date": "2026-10-21"},
					"end":   map[string]string{"date": "2026-10-22"},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	src := &GoogleSource{BaseURL: srv.URL, HTTP: srv.Client()}
	page, err := src.ListEvents(context.Background(), "tok-abc", Query{
		TimeMin:    time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
		TimeMax:    time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC),
		MaxResults: 10,
		PageToken:  "page-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Fatalf("auth = %q", gotAuth)
	}
	for _, want := range []string{"singleEvents=true", "orderBy=startTime", "maxResults=10", "pageToken=page-1", "timeMin=", "timeMax="} {
		if !strings.Contains(gotURL, want) {
			t.Fatalf("url missing %q: %s", want, gotURL)
		}
	}
	if page.NextPageToken != "page-2" || len(page.Events) != 3 {
		t.Fatalf("page = %+v", page)
	}
	e0 := page.Events[0]
	if e0.Title != "Standup" || e0.Location != "Zoom" || e0.Timezone != "America/Bogota" || e0.AllDay {
		t.Fatalf("e0 = %+v", e0)
	}
	if e0.Organizer == nil || e0.Organizer.Email != "a@ex.com" || e0.Organizer.Name != "Ada" {
		t.Fatalf("organizer = %+v", e0.Organizer)
	}
	e1 := page.Events[1]
	if e1.Title != "" || e1.Timezone != "America/Bogota" {
		t.Fatalf("e1 should inherit cal tz and empty title: %+v", e1)
	}
	e2 := page.Events[2]
	if !e2.AllDay || e2.Start != "2026-10-21" || e2.End != "2026-10-22" {
		t.Fatalf("all-day = %+v", e2)
	}
}

func TestGoogleListEventsErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		header map[string]string
		want   error
		rate   bool
	}{
		{"401", 401, `{}`, nil, ErrTokenRejected, false},
		{"404", 404, `{}`, nil, ErrCalendarNotFound, false},
		{"429", 429, `{}`, map[string]string{"Retry-After": "12"}, nil, true},
		{"403 rate", 403, `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`, nil, nil, true},
		{"500", 500, `{}`, nil, ErrProviderUnavailable, false},
		{"400", 400, `{}`, nil, ErrProviderBadResponse, false},
		{"bad json", 200, `{`, nil, ErrProviderBadResponse, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)
			src := &GoogleSource{BaseURL: srv.URL, HTTP: srv.Client()}
			_, err := src.ListEvents(context.Background(), "t", Query{
				TimeMin: time.Now(), TimeMax: time.Now().Add(time.Hour),
			})
			if tt.rate {
				var rl *RateLimitedError
				if !errors.As(err, &rl) {
					t.Fatalf("err = %v", err)
				}
				if tt.header["Retry-After"] == "12" && rl.RetryAfter != 12*time.Second {
					t.Fatalf("retry = %v", rl.RetryAfter)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestGoogleListEventsBodyTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write(make([]byte, maxGoogleBody+2))
	}))
	t.Cleanup(srv.Close)
	src := &GoogleSource{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := src.ListEvents(context.Background(), "t", Query{
		TimeMin: time.Now(), TimeMax: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, ErrProviderBadResponse) {
		t.Fatalf("err = %v", err)
	}
}
