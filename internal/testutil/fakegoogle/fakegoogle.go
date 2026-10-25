// Package fakegoogle is a stateful Google stand-in for tests: OAuth (with
// PKCE, one-time codes, refresh/access validity and revocation, from
// fakeoauth) plus the Calendar API events endpoint.
//
// It does not import Relay's oauth or calendar packages (avoids import
// cycles). Callers wire its URLs into oauth2.Config / GoogleSource.
package fakegoogle

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/testutil/fakeoauth"
)

const (
	ScopeOpenID           = "openid"
	ScopeEmail            = "email"
	ScopeCalendarReadonly = "https://www.googleapis.com/auth/calendar.readonly"
)

// Options configures the fake Google.
type Options struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Sub          string        // id_token sub / provider_account_id
	Email        string        // optional verified email in the id_token
	AccessTTL    time.Duration // default 1h
}

// Event is a calendar event returned by the fake Calendar API.
type Event struct {
	ID             string
	Summary        string
	Start          string // RFC3339 or YYYY-MM-DD
	End            string
	AllDay         bool
	Location       string
	TimeZone       string
	OrganizerEmail string
	OrganizerName  string
	ETag           string
	Status         string // "cancelled" keeps the event in a sync snapshot as a delete
}

// RequestRecord is the last observed token/calendar request (for asserts).
type RequestRecord = fakeoauth.RequestRecord

// Calls are counters of token-endpoint successes and events attempts.
type Calls = fakeoauth.Calls

// Server is a fake Google OAuth + Calendar API.
type Server struct {
	*fakeoauth.Core
	opts      Options
	srv       *httptest.Server
	mu        sync.Mutex
	events    []Event
	revokes   atomic.Int64
	watches   map[string]watchChannel
	last      watchChannel
	watchN    atomic.Int64
	stopN     atomic.Int64
	syncSnap  map[string]Event
	syncToken string
	syncGone  bool
	lastSync  string
}

// New starts the fake server. Closed via t.Cleanup.
func New(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.ClientID == "" {
		opts.ClientID = "client-test"
	}
	if opts.ClientSecret == "" {
		opts.ClientSecret = "secret-test"
	}
	if opts.RedirectURL == "" {
		opts.RedirectURL = "http://localhost:8080/v1/oauth/google/callback"
	}
	if opts.Sub == "" {
		opts.Sub = "sub-fake-google"
	}
	g := &Server{opts: opts, watches: map[string]watchChannel{}}
	g.Core = fakeoauth.New(t, fakeoauth.Config{
		Name:         "fakegoogle",
		ClientID:     opts.ClientID,
		ClientSecret: opts.ClientSecret,
		RedirectURL:  opts.RedirectURL,
		AccessTTL:    opts.AccessTTL,
		ValidateAuthURL: func(t *testing.T, q url.Values) {
			if q.Get("access_type") != "offline" {
				t.Errorf("fakegoogle Consent: access_type = %q, want offline", q.Get("access_type"))
			}
			scope := q.Get("scope")
			if !strings.Contains(scope, ScopeOpenID) || !strings.Contains(scope, ScopeCalendarReadonly) {
				t.Errorf("fakegoogle Consent: scope = %q", scope)
			}
		},
		TokenExtras: func(exp time.Time) map[string]any {
			return map[string]any{
				"scope":    ScopeOpenID + " " + ScopeEmail + " " + ScopeCalendarReadonly,
				"id_token": g.idToken(exp),
			}
		},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/o/oauth2/auth", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "use Consent/Deny helpers", http.StatusNotImplemented)
	})
	mux.HandleFunc("/token", g.HandleToken)
	mux.HandleFunc("/revoke", g.handleRevoke)
	mux.HandleFunc("/calendars/primary/events", g.handleEvents)
	mux.HandleFunc("POST /calendars/primary/events/watch", g.handleWatch)
	mux.HandleFunc("POST /channels/stop", g.handleStop)
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *Server) TokenURL() string     { return g.srv.URL + "/token" }
func (g *Server) AuthURL() string      { return g.srv.URL + "/o/oauth2/auth" }
func (g *Server) RevokeURL() string    { return g.srv.URL + "/revoke" }
func (g *Server) APIBaseURL() string   { return g.srv.URL }
func (g *Server) Client() *http.Client { return g.srv.Client() }
func (g *Server) Sub() string          { return g.opts.Sub }

// AccountID is the provider_account_id Relay stores (the id_token sub).
func (g *Server) AccountID() string { return g.opts.Sub }

// Revokes counts successful calls to the revocation endpoint.
func (g *Server) Revokes() int64 { return g.revokes.Load() }

// SetEvents replaces the calendar feed.
func (g *Server) SetEvents(evs ...Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append([]Event(nil), evs...)
}

// handleRevoke is RFC 7009 as Google implements it: unknown tokens get
// 400 invalid_token.
func (g *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	if !g.RevokeToken(r.PostForm.Get("token")) {
		fakeoauth.WriteOAuthError(w, "invalid_token")
		return
	}
	g.revokes.Add(1)
	w.WriteHeader(http.StatusOK)
}

func (g *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	g.CountEvents() // every attempt, including 401s
	if g.TakeFail("events", w) {
		return
	}
	g.Record(RequestRecord{Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone()})
	if !g.AccessOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	q := r.URL.Query()
	// Delta sync does not send orderBy. The live list always does.
	if q.Get("syncToken") != "" || q.Get("orderBy") == "" {
		g.handleSync(w, q)
		return
	}
	if q.Get("singleEvents") != "true" || q.Get("orderBy") != "startTime" {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	timeMin, err1 := time.Parse(time.RFC3339, q.Get("timeMin"))
	timeMax, err2 := time.Parse(time.RFC3339, q.Get("timeMax"))
	if err1 != nil || err2 != nil {
		http.Error(w, "bad time window", http.StatusBadRequest)
		return
	}
	maxResults := 250
	if raw := q.Get("maxResults"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			http.Error(w, "bad maxResults", http.StatusBadRequest)
			return
		}
		maxResults = n
	}

	g.mu.Lock()
	evs := append([]Event(nil), g.events...)
	g.mu.Unlock()

	items := make([]map[string]any, 0)
	for _, e := range evs {
		if !eventInWindow(e, timeMin, timeMax) {
			continue
		}
		item := map[string]any{"id": e.ID, "summary": e.Summary}
		if e.Location != "" {
			item["location"] = e.Location
		}
		if e.AllDay {
			item["start"] = map[string]string{"date": e.Start}
			item["end"] = map[string]string{"date": e.End}
		} else {
			start := map[string]string{"dateTime": e.Start}
			end := map[string]string{"dateTime": e.End}
			if e.TimeZone != "" {
				start["timeZone"] = e.TimeZone
				end["timeZone"] = e.TimeZone
			}
			item["start"] = start
			item["end"] = end
		}
		if e.OrganizerEmail != "" || e.OrganizerName != "" {
			item["organizer"] = map[string]string{"email": e.OrganizerEmail, "displayName": e.OrganizerName}
		}
		items = append(items, item)
		if len(items) >= maxResults {
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"timeZone": "UTC", "items": items})
}

// ExpireSync makes the next incremental sync answer 410. A full sync
// (no syncToken) still succeeds and issues a new token.
func (g *Server) ExpireSync() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.syncGone = true
}

// LastSyncQuery is the query string of the last sync request.
func (g *Server) LastSyncQuery() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastSync
}

func (g *Server) handleSync(w http.ResponseWriter, q url.Values) {
	if q.Get("singleEvents") != "true" {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastSync = q.Encode()

	if tok := q.Get("syncToken"); tok != "" {
		if g.syncGone || tok != g.syncToken {
			g.syncGone = false
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": 410, "message": "Sync token is no longer valid"},
			})
			return
		}
		g.writeSyncPage(w, q, g.diffSync(g.events))
		return
	}

	timeMin, err1 := time.Parse(time.RFC3339, q.Get("timeMin"))
	timeMax, err2 := time.Parse(time.RFC3339, q.Get("timeMax"))
	if err1 != nil || err2 != nil {
		http.Error(w, "bad time window", http.StatusBadRequest)
		return
	}
	var items []Event
	for _, e := range g.events {
		if e.Status == "cancelled" {
			continue
		}
		if eventInWindow(e, timeMin, timeMax) {
			items = append(items, e)
		}
	}
	g.writeSyncPage(w, q, items)
}

func (g *Server) diffSync(current []Event) []Event {
	live := map[string]Event{}
	for _, e := range current {
		if e.Status == "cancelled" {
			continue
		}
		live[e.ID] = e
	}
	var out []Event
	for id, old := range g.syncSnap {
		cur, ok := live[id]
		if !ok {
			old.Status = "cancelled"
			old.ETag = "gone-" + eventETag(old)
			out = append(out, old)
			continue
		}
		if eventETag(cur) != eventETag(old) {
			out = append(out, cur)
		}
		delete(live, id)
	}
	for _, e := range live {
		out = append(out, e)
	}
	return out
}

func (g *Server) writeSyncPage(w http.ResponseWriter, q url.Values, items []Event) {
	maxResults := 250
	if raw := q.Get("maxResults"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.Error(w, "bad maxResults", http.StatusBadRequest)
			return
		}
		maxResults = n
	}
	offset := 0
	if raw := q.Get("pageToken"); raw != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(raw, "p"))
		if err != nil || n < 0 {
			http.Error(w, "bad pageToken", http.StatusBadRequest)
			return
		}
		offset = n
	}
	body := map[string]any{"timeZone": "UTC", "items": []any{}}
	if offset < len(items) {
		end := offset + maxResults
		if end > len(items) {
			end = len(items)
		}
		page := items[offset:end]
		encoded := make([]any, 0, len(page))
		for _, e := range page {
			encoded = append(encoded, googleSyncItem(e))
		}
		body["items"] = encoded
		if end < len(items) {
			body["nextPageToken"] = fmt.Sprintf("p%d", end)
		}
	}
	if body["nextPageToken"] == nil {
		token := fmt.Sprintf("sync-%d", time.Now().UnixNano())
		g.syncToken = token
		g.syncGone = false
		g.syncSnap = map[string]Event{}
		for _, e := range g.events {
			if e.Status != "cancelled" {
				g.syncSnap[e.ID] = e
			}
		}
		body["nextSyncToken"] = token
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func googleSyncItem(e Event) map[string]any {
	item := map[string]any{
		"id": e.ID, "summary": e.Summary, "etag": eventETag(e), "status": "confirmed",
	}
	if e.Status == "cancelled" {
		item["status"] = "cancelled"
	}
	if e.Location != "" {
		item["location"] = e.Location
	}
	if e.AllDay {
		item["start"] = map[string]string{"date": e.Start}
		item["end"] = map[string]string{"date": e.End}
	} else if e.Start != "" {
		start := map[string]string{"dateTime": e.Start}
		end := map[string]string{"dateTime": e.End}
		if e.TimeZone != "" {
			start["timeZone"] = e.TimeZone
			end["timeZone"] = e.TimeZone
		}
		item["start"] = start
		item["end"] = end
	}
	if e.OrganizerEmail != "" || e.OrganizerName != "" {
		item["organizer"] = map[string]string{"email": e.OrganizerEmail, "displayName": e.OrganizerName}
	}
	return item
}

func eventETag(e Event) string {
	if e.ETag != "" && e.Status != "cancelled" {
		return e.ETag
	}
	if e.Status == "cancelled" && e.ETag != "" {
		return e.ETag
	}
	return fmt.Sprintf("%s|%s|%s|%s|%t", e.ID, e.Summary, e.Start, e.End, e.AllDay)
}

func (g *Server) idToken(exp time.Time) string {
	claims := map[string]any{
		"iss": "https://accounts.google.com",
		"aud": g.opts.ClientID,
		"sub": g.opts.Sub,
		"exp": exp.Unix(),
	}
	if g.opts.Email != "" {
		claims["email"] = g.opts.Email
		claims["email_verified"] = true
	}
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

type watchChannel struct {
	ID, ResourceID, Token, Address string
	Expiration                     time.Time
}

// Watch is the last channel events.watch created.
type Watch struct {
	ID, ResourceID, Token, Address string
}

// Watches counts successful events.watch calls.
func (g *Server) Watches() int64 { return g.watchN.Load() }

// Stops counts successful channels.stop calls.
func (g *Server) Stops() int64 { return g.stopN.Load() }

// LastWatch returns the most recently created channel.
func (g *Server) LastWatch() Watch {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Watch{ID: g.last.ID, ResourceID: g.last.ResourceID, Token: g.last.Token, Address: g.last.Address}
}

func (g *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	g.Record(RequestRecord{Path: r.URL.Path, Header: r.Header.Clone()})
	if g.TakeFail("watch", w) {
		return
	}
	if !g.AccessOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Address    string `json:"address"`
		Token      string `json:"token"`
		Expiration int64  `json:"expiration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.ID == "" || body.Type != "web_hook" || body.Token == "" || !strings.HasSuffix(body.Address, "/v1/webhooks/google") {
		http.Error(w, "bad watch", http.StatusBadRequest)
		return
	}
	exp := time.UnixMilli(body.Expiration).UTC()
	if body.Expiration == 0 {
		exp = time.Now().Add(7 * 24 * time.Hour).UTC()
	}
	ch := watchChannel{ID: body.ID, ResourceID: "resource-" + body.ID, Token: body.Token, Address: body.Address, Expiration: exp}
	g.mu.Lock()
	g.watches[body.ID] = ch
	g.last = ch
	g.mu.Unlock()
	g.watchN.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": ch.ID, "resourceId": ch.ResourceID, "expiration": ch.Expiration.UnixMilli(),
	})
}

func (g *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if g.TakeFail("stop", w) {
		return
	}
	if !g.AccessOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		ID         string `json:"id"`
		ResourceID string `json:"resourceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	ch, ok := g.watches[body.ID]
	if ok && ch.ResourceID == body.ResourceID {
		delete(g.watches, body.ID)
	}
	g.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	g.stopN.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

func eventInWindow(e Event, min, max time.Time) bool {
	layout := time.RFC3339
	if e.AllDay {
		layout = time.DateOnly
	}
	start, err := time.Parse(layout, e.Start)
	if err != nil {
		return true // keep malformed events visible in tests
	}
	return !start.Before(min) && start.Before(max)
}
