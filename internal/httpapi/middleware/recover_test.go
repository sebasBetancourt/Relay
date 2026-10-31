package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoverCatchesPanic(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	boom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := Recover(log)(boom)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q: a panic must answer like any other error", got)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"internal_error"}` {
		t.Fatalf("body = %s", got)
	}
}
