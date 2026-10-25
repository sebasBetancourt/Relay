package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMicrosoftDeltaFollowsSameHostOnly(t *testing.T) {
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		evilHits.Add(1)
	}))
	defer evil.Close()

	var calls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch calls {
		case 1:
			if r.URL.Query().Get("startDateTime") == "" {
				t.Errorf("full delta query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"a","changeKey":"k1","subject":"A",
				"start":{"dateTime":"2026-11-01T15:00:00.0000000","timeZone":"UTC"},
				"end":{"dateTime":"2026-11-01T16:00:00.0000000","timeZone":"UTC"}}],
				"@odata.nextLink":"` + srv.URL + `/v1.0/me/calendarView/delta?$skiptoken=2"}`))
		case 2:
			if r.URL.Query().Get("$skiptoken") != "2" {
				t.Errorf("page query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"value":[{"id":"b","@removed":{"reason":"deleted"}}],
				"@odata.deltaLink":"` + srv.URL + `/v1.0/me/calendarView/delta?$deltatoken=d1"}`))
		default:
			t.Errorf("unexpected call %s", r.URL.RequestURI())
		}
	}))
	defer srv.Close()

	m := &Microsoft{BaseURL: srv.URL, HTTP: srv.Client()}
	window := Window{Start: time.Now().UTC(), End: time.Now().UTC().Add(time.Hour)}
	changes, next, err := m.Delta(context.Background(), "tok", "", window)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(changes) != 2 || !changes[1].Deleted || changes[0].Version != "k1" {
		t.Fatalf("calls %d changes %+v next %q", calls, changes, next)
	}
	if next == "" || evilHits.Load() != 0 {
		t.Fatalf("next %q evil %d", next, evilHits.Load())
	}

	foreign := &Microsoft{BaseURL: srv.URL, HTTP: srv.Client()}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":[],"@odata.nextLink":"` + evil.URL + `/v1.0/me/calendarView/delta?$skiptoken=x"}`))
	}))
	defer bad.Close()
	foreign.BaseURL = bad.URL
	if _, _, err := foreign.Delta(context.Background(), "tok", "", window); err == nil {
		t.Fatal("foreign nextLink was accepted")
	}
	if evilHits.Load() != 0 {
		t.Fatal("foreign host was called")
	}

	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer gone.Close()
	cursor := gone.URL + "/v1.0/me/calendarView/delta?$deltatoken=old"
	gm := &Microsoft{BaseURL: gone.URL, HTTP: gone.Client()}
	if _, _, err := gm.Delta(context.Background(), "tok", cursor, window); err != ErrCursorExpired {
		t.Fatalf("410 err = %v", err)
	}
}
