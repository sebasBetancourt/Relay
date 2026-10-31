package fakegoogle

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Write is one insert, patch or delete the fake accepted, kept exactly as it
// arrived so tests assert what Relay sent, not what it meant to send.
type Write struct {
	Method  string
	ID      string
	Query   url.Values
	IfMatch string
	Body    map[string]any
}

// Writes returns every accepted write, oldest first.
func (g *Server) Writes() []Write {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Write(nil), g.writes...)
}

func writeError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": status, "errors": []map[string]string{{"reason": reason}}},
	})
}

// authorized applies Google's order: a bad token is 401, a good token without
// the write scope is 403 insufficientPermissions.
func (g *Server) authorized(w http.ResponseWriter, r *http.Request, write bool) bool {
	if g.TakeFail("write", w) {
		return false
	}
	if !g.AccessOK(r) {
		writeError(w, http.StatusUnauthorized, "authError")
		return false
	}
	if write && !g.HasScope(r, ScopeCalendarEvents) {
		writeError(w, http.StatusForbidden, "insufficientPermissions")
		return false
	}
	return true
}

func (g *Server) handleInsert(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(w, r, true) {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "parseError")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writeSeq++
	e := Event{ID: fmt.Sprintf("created-%d", g.writeSeq), ETag: fmt.Sprintf(`"etag-%d"`, g.writeSeq)}
	if reason := applyBody(&e, body); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	g.events = append(g.events, e)
	g.writes = append(g.writes, Write{Method: r.Method, ID: e.ID, Query: r.URL.Query(), Body: body})
	writeEvent(w, e)
}

func (g *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(w, r, false) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	i := g.indexOf(r.PathValue("id"))
	if i < 0 {
		writeError(w, http.StatusNotFound, "notFound")
		return
	}
	writeEvent(w, g.events[i])
}

func (g *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(w, r, true) {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "parseError")
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	i := g.indexOf(r.PathValue("id"))
	if i < 0 {
		writeError(w, http.StatusNotFound, "notFound")
		return
	}
	ifMatch := r.Header.Get("If-Match")
	if ifMatch != "" && ifMatch != g.events[i].ETag {
		writeError(w, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	e := g.events[i]
	if reason := applyBody(&e, body); reason != "" {
		writeError(w, http.StatusBadRequest, reason)
		return
	}
	g.writeSeq++
	e.ETag = fmt.Sprintf(`"etag-%d"`, g.writeSeq)
	g.events[i] = e
	g.writes = append(g.writes, Write{Method: r.Method, ID: e.ID, Query: r.URL.Query(), IfMatch: ifMatch, Body: body})
	writeEvent(w, e)
}

func (g *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !g.authorized(w, r, true) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	i := g.indexOf(r.PathValue("id"))
	if i < 0 {
		writeError(w, http.StatusGone, "deleted")
		return
	}
	g.events = append(g.events[:i], g.events[i+1:]...)
	g.writes = append(g.writes, Write{Method: r.Method, ID: r.PathValue("id"), Query: r.URL.Query()})
	w.WriteHeader(http.StatusNoContent)
}

func (g *Server) indexOf(id string) int {
	for i, e := range g.events {
		if e.ID == id {
			return i
		}
	}
	return -1
}

// applyBody merges fields the way events.patch does. It returns Google's error
// reason for a body the real API refuses.
func applyBody(e *Event, body map[string]any) string {
	if v, ok := body["summary"].(string); ok {
		e.Summary = v
	}
	if v, ok := body["location"].(string); ok {
		e.Location = v
	}
	if raw, ok := body["recurrence"].([]any); ok {
		e.Recurrence = nil
		for _, r := range raw {
			line, _ := r.(string)
			if !strings.HasPrefix(line, "RRULE:") {
				return "invalidRecurrence"
			}
			e.Recurrence = append(e.Recurrence, line)
		}
	}
	start, hasStart := body["start"].(map[string]any)
	end, hasEnd := body["end"].(map[string]any)
	if hasStart != hasEnd {
		return "invalid"
	}
	if !hasStart {
		return ""
	}
	if date, ok := start["date"].(string); ok && date != "" {
		if start["dateTime"] != nil {
			return "invalid" // a real event cannot hold both
		}
		e.AllDay, e.TimeZone, e.Start = true, "", date
		e.End, _ = end["date"].(string)
		return ""
	}
	dateTime, _ := start["dateTime"].(string)
	zone, _ := start["timeZone"].(string)
	if dateTime == "" || (len(e.Recurrence) > 0 && zone == "") {
		return "invalid" // a series needs a time zone to expand
	}
	e.AllDay, e.TimeZone, e.Start = false, zone, dateTime
	e.End, _ = end["dateTime"].(string)
	return ""
}

func writeEvent(w http.ResponseWriter, e Event) {
	item := map[string]any{
		"id": e.ID, "etag": eventETag(e), "summary": e.Summary, "status": "confirmed",
		"location": e.Location,
	}
	if e.AllDay {
		item["start"] = map[string]string{"date": e.Start}
		item["end"] = map[string]string{"date": e.End}
	} else {
		item["start"] = map[string]string{"dateTime": e.Start, "timeZone": e.TimeZone}
		item["end"] = map[string]string{"dateTime": e.End, "timeZone": e.TimeZone}
	}
	if len(e.Recurrence) > 0 {
		item["recurrence"] = e.Recurrence
	}
	if e.RecurringEventID != "" {
		item["recurringEventId"] = e.RecurringEventID
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(item)
}
