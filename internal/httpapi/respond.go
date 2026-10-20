package httpapi

import (
	"encoding/json"
	"net/http"
)

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// The body is never HTML: tell browsers not to sniff it as such.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// Keep "&", "<" and ">" literal. The default \u0026 escaping is valid JSON
	// but breaks URLs a human copies from the response (authorization_url).
	enc.SetEscapeHTML(false)
	// Headers are already sent; an encode error here can only be a broken connection.
	_ = enc.Encode(v)
}
