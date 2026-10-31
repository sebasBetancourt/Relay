package fakemicrosoft

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	scopeWrite     = "Calendars.ReadWrite"
	wallClock      = "2006-01-02T15:04:05"
	graphResource  = "https://graph.microsoft.com/"
	preferUTC      = `outlook.timezone="UTC"`
	typeSeriesHead = "seriesMaster"
)

// fakeZones is deliberately small: a zone outside it is a test that needs one
// more line, while Graph refusing an IANA name here catches a missed translation.
var fakeZones = map[string]string{
	"UTC":                      "UTC",
	"SA Pacific Standard Time": "America/Bogota",
	"Eastern Standard Time":    "America/New_York",
	"Romance Standard Time":    "Europe/Paris",
}

// Write is one create, patch or delete the fake accepted, kept exactly as it
// arrived so tests assert what Relay sent, not what it meant to send.
type Write struct {
	Method  string
	ID      string
	IfMatch string
	Body    map[string]any
}

// Writes returns every accepted write, oldest first.
func (m *Server) Writes() []Write {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Write(nil), m.writes...)
}

// graphGrantedScope returns scopes as Entra ID does: Graph ones as full URIs.
func graphGrantedScope(consented string) string {
	fields := strings.Fields(consented)
	for i, f := range fields {
		if strings.Contains(f, ".") {
			fields[i] = graphResource + f
		}
	}
	return strings.Join(fields, " ")
}

func (m *Server) authorized(w http.ResponseWriter, r *http.Request, write bool) bool {
	if m.TakeFail("write", w) {
		return false
	}
	if !m.AccessOK(r) {
		writeGraphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return false
	}
	if write && !m.HasScope(r, scopeWrite) {
		writeGraphError(w, http.StatusForbidden, "ErrorAccessDenied")
		return false
	}
	return true
}

func (m *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(w, r, true) {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeGraphError(w, http.StatusBadRequest, "ErrorInvalidRequest")
		return
	}
	if r.Header.Get("Prefer") != preferUTC {
		writeGraphError(w, http.StatusBadRequest, "fake: Relay must ask for UTC")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeSeq++
	e := Event{ID: fmt.Sprintf("created-%d", m.writeSeq)}
	if code := applyBody(&e, body); code != "" {
		writeGraphError(w, http.StatusBadRequest, code)
		return
	}
	m.events = append(m.events, e)
	m.writes = append(m.writes, Write{Method: r.Method, ID: e.ID, Body: body})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(graphEvent(e))
}

func (m *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(w, r, false) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.indexOf(r.PathValue("id"))
	if i < 0 {
		writeGraphError(w, http.StatusNotFound, "ErrorItemNotFound")
		return
	}
	e := m.events[i]
	kind := e.Type
	if kind == "" {
		kind = "singleInstance"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": e.ID, "type": kind})
}

func (m *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(w, r, true) {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeGraphError(w, http.StatusBadRequest, "ErrorInvalidRequest")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.indexOf(r.PathValue("id"))
	if i < 0 {
		writeGraphError(w, http.StatusNotFound, "ErrorItemNotFound")
		return
	}
	ifMatch := r.Header.Get("If-Match")
	if ifMatch != "" && ifMatch != `W/"`+changeKey(m.events[i])+`"` {
		writeGraphError(w, http.StatusPreconditionFailed, "ErrorIrresolvableConflict")
		return
	}
	e := m.events[i]
	if code := applyBody(&e, body); code != "" {
		writeGraphError(w, http.StatusBadRequest, code)
		return
	}
	m.events[i] = e
	m.writes = append(m.writes, Write{Method: r.Method, ID: e.ID, IfMatch: ifMatch, Body: body})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(graphEvent(e))
}

func (m *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(w, r, true) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.indexOf(r.PathValue("id"))
	if i < 0 {
		writeGraphError(w, http.StatusNotFound, "ErrorItemNotFound")
		return
	}
	m.events = append(m.events[:i], m.events[i+1:]...)
	m.writes = append(m.writes, Write{Method: r.Method, ID: r.PathValue("id")})
	w.WriteHeader(http.StatusNoContent)
}

func (m *Server) indexOf(id string) int {
	for i, e := range m.events {
		if e.ID == id {
			return i
		}
	}
	return -1
}

// applyBody merges fields and returns Graph's error code for a body it refuses.
func applyBody(e *Event, body map[string]any) string {
	if v, ok := body["subject"].(string); ok {
		e.Subject = v
	}
	if loc, ok := body["location"].(map[string]any); ok {
		e.Location, _ = loc["displayName"].(string)
	}
	if v, ok := body["isAllDay"].(bool); ok {
		e.AllDay = v
	}
	if _, ok := body["recurrence"]; ok {
		e.Type = typeSeriesHead
	}
	start, hasStart := body["start"].(map[string]any)
	end, hasEnd := body["end"].(map[string]any)
	if hasStart != hasEnd {
		return "ErrorInvalidRequest"
	}
	if !hasStart {
		return ""
	}
	st, zone, code := parseWallClock(start)
	if code != "" {
		return code
	}
	en, _, code := parseWallClock(end)
	if code != "" {
		return code
	}
	if e.AllDay && (st.Hour() != 0 || en.Hour() != 0) {
		return "ErrorInvalidRequest" // all-day events must span midnight to midnight
	}
	e.Start, e.End, e.TimeZone = st, en, zone
	return ""
}

func parseWallClock(raw map[string]any) (time.Time, string, string) {
	value, _ := raw["dateTime"].(string)
	zone, _ := raw["timeZone"].(string)
	iana, ok := fakeZones[zone]
	if !ok {
		return time.Time{}, "", "ErrorTimeZoneNotSupported"
	}
	loc, err := time.LoadLocation(iana)
	if err != nil {
		return time.Time{}, "", "ErrorTimeZoneNotSupported"
	}
	// No offset: Graph reads the value in timeZone.
	t, err := time.ParseInLocation(wallClock, value, loc)
	if err != nil {
		return time.Time{}, "", "ErrorInvalidRequest"
	}
	return t, zone, ""
}
