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
}

// RequestRecord is the last observed token/calendar request (for asserts).
type RequestRecord = fakeoauth.RequestRecord

// Calls are counters of token-endpoint successes and events attempts.
type Calls = fakeoauth.Calls

// Server is a fake Google OAuth + Calendar API.
type Server struct {
	*fakeoauth.Core
	opts    Options
	srv     *httptest.Server
	mu      sync.Mutex
	events  []Event
	revokes atomic.Int64
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
	g := &Server{opts: opts}
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
