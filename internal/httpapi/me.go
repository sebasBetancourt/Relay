package httpapi

import (
	"net/http"

	"github.com/sebasBetancourt/relay/internal/httpapi/middleware"
)

// handleMe returns the calling tenant: the simplest end-to-end check that
// an API key works.
func handleMe(w http.ResponseWriter, r *http.Request) {
	t, ok := middleware.TenantFromContext(r.Context())
	if !ok {
		// Only possible if the route was mounted without RequireAPIKey.
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal_error"})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		KeyPrefix string `json:"key_prefix"`
	}{t.ID, t.Name, t.KeyPrefix})
}
