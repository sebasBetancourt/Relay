package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGoogleDeltaPagesCancelledAndGone(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("syncToken") == "stale-token-value" {
			http.Error(w, "gone", http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "" && r.URL.Query().Get("syncToken") == "" {
			if r.URL.Query().Get("timeMin") == "" || r.URL.Query().Get("orderBy") != "" {
				t.Errorf("full sync query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{
				"timeZone":"UTC",
				"nextPageToken":"p2",
				"items":[{"id":"a","etag":"v1","status":"confirmed","summary":"A",
					"start":{"dateTime":"2026-11-01T15:00:00Z"},"end":{"dateTime":"2026-11-01T16:00:00Z"}}]
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"nextSyncToken":"sync-1",
			"items":[
				{"id":"series_20261101T150000Z","etag":"v1","status":"confirmed","summary":"Instance",
					"start":{"dateTime":"2026-11-01T15:00:00Z"},"end":{"dateTime":"2026-11-01T16:00:00Z"}},
				{"id":"a","etag":"v2","status":"cancelled"}
			]
		}`))
	}))
	defer srv.Close()

	g := &Google{BaseURL: srv.URL, HTTP: srv.Client()}
	window := Window{Start: time.Now().UTC(), End: time.Now().UTC().Add(time.Hour)}
	changes, next, err := g.Delta(context.Background(), "tok", "", window)
	if err != nil {
		t.Fatal(err)
	}
	if next != "sync-1" || calls != 2 {
		t.Fatalf("next %q calls %d", next, calls)
	}
	if len(changes) != 3 || !changes[2].Deleted || changes[2].Version != "v2" {
		t.Fatalf("changes = %+v", changes)
	}
	if changes[1].Event.ID != "series_20261101T150000Z" || changes[1].Event.Title != "Instance" {
		t.Fatalf("instance = %+v", changes[1].Event)
	}
	_, _, err = g.Delta(context.Background(), "tok", "stale-token-value", window)
	if err != ErrCursorExpired {
		t.Fatalf("expired err = %v", err)
	}
	if strings.Contains(err.Error(), "stale-token-value") {
		t.Fatal("error must not echo the cursor")
	}
}
