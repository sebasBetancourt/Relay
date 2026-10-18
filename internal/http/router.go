package http

import (
	"log/slog"
	"net/http"
)

func NewRouter(log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	// Outer runs first. RequestID must wrap Recover/Logger so they see the id in context.
	return RequestID(Recover(log)(Logger(log)(mux)))
}
