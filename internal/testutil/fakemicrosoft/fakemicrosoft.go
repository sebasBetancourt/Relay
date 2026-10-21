// Package fakemicrosoft is a stateful Microsoft stand-in for tests: Entra ID
// (OAuth with PKCE, from fakeoauth) plus Microsoft Graph /me and
// /me/calendarView.
//
// It is stricter than Microsoft on purpose:
//   - every refresh rotates the refresh token and the one used stops working,
//     so a client that fails to store the new one is caught at once;
//   - the client secret must travel in the request body (no Basic auth),
//     matching how Relay is configured.
//
// @odata.nextLink points at the real Graph host, never at this server: a
// client that followed the link instead of keeping only $skiptoken would
// leave the test sandbox and fail.
package fakemicrosoft

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/testutil/fakeoauth"
)

// Options configures the fake Microsoft.
type Options struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Tenant       string // authority segment, default "common"
	UserID       string // Graph /me id → provider_account_id
	Email        string // /me userPrincipalName (mail is left null, like personal accounts)
	AccessTTL    time.Duration
}

// Event is a calendar event served by the fake calendarView.
type Event struct {
	ID             string
	Subject        string
	Start          time.Time // timed: instant; all-day: the date (any time of day)
	End            time.Time
	AllDay         bool
	Cancelled      bool
	TimeZone       string // originalStartTimeZone, e.g. "SA Pacific Standard Time"
	Location       string
	OrganizerEmail string
	OrganizerName  string
}

// Calls counts token, /me and calendarView traffic.
type Calls = fakeoauth.Calls

// Server is a fake Entra ID + Microsoft Graph.
type Server struct {
	*fakeoauth.Core
	opts   Options
	srv    *httptest.Server
	mu     sync.Mutex
	events []Event
	noBox  bool
	meN    atomic.Int64
}

// New starts the fake server. Closed via t.Cleanup.
func New(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.ClientID == "" {
		opts.ClientID = "ms-client-test"
	}
	if opts.ClientSecret == "" {
		opts.ClientSecret = "ms-secret-test"
	}
	if opts.RedirectURL == "" {
		opts.RedirectURL = "http://localhost:8080/v1/oauth/microsoft/callback"
	}
	if opts.Tenant == "" {
		opts.Tenant = "common"
	}
	if opts.UserID == "" {
		opts.UserID = "ms-user-fake"
	}
	m := &Server{opts: opts}
	m.Core = fakeoauth.New(t, fakeoauth.Config{
		Name:             "fakemicrosoft",
		ClientID:         opts.ClientID,
		ClientSecret:     opts.ClientSecret,
		RedirectURL:      opts.RedirectURL,
		AccessTTL:        opts.AccessTTL,
		RotateRefresh:    true,
		SecretInBodyOnly: true,
		ValidateAuthURL: func(t *testing.T, q url.Values) {
			scopes := strings.Fields(q.Get("scope"))
			for _, want := range []string{"openid", "offline_access", "User.Read", "Calendars.Read"} {
				if !slices.Contains(scopes, want) {
					t.Errorf("fakemicrosoft Consent: scope %q lacks %s", q.Get("scope"), want)
				}
			}
			if q.Has("access_type") {
				t.Errorf("fakemicrosoft Consent: access_type is a Google parameter")
			}
		},
		TokenExtras: func(time.Time) map[string]any {
			// Entra ID returns Graph scopes as full resource URIs.
			return map[string]any{
				"scope": "https://graph.microsoft.com/Calendars.Read https://graph.microsoft.com/User.Read openid email offline_access",
			}
		},
	})

	mux := http.NewServeMux()
	base := "/" + opts.Tenant + "/oauth2/v2.0"
	mux.HandleFunc(base+"/authorize", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "use Consent/Deny helpers", http.StatusNotImplemented)
	})
	mux.HandleFunc(base+"/token", m.HandleToken)
	mux.HandleFunc("GET /v1.0/me", m.handleMe)
	mux.HandleFunc("GET /v1.0/me/calendarView", m.handleCalendarView)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *Server) AuthURL() string {
	return m.srv.URL + "/" + m.opts.Tenant + "/oauth2/v2.0/authorize"
}
func (m *Server) TokenURL() string { return m.srv.URL + "/" + m.opts.Tenant + "/oauth2/v2.0/token" }

// GraphBaseURL replaces https://graph.microsoft.com.
func (m *Server) GraphBaseURL() string { return m.srv.URL }
func (m *Server) Client() *http.Client { return m.srv.Client() }

// AccountID is the provider_account_id Relay stores (the Graph /me id).
func (m *Server) AccountID() string { return m.opts.UserID }

// MeCalls counts GET /me requests.
func (m *Server) MeCalls() int64 { return m.meN.Load() }

// SetEvents replaces the calendar.
func (m *Server) SetEvents(evs ...Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append([]Event(nil), evs...)
}

// MailboxNotEnabled makes calendarView answer like a Microsoft account
// without an Outlook mailbox.
func (m *Server) MailboxNotEnabled(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noBox = on
}

func (m *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	m.meN.Add(1)
	if !m.AccessOK(r) {
		writeGraphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	if r.URL.Query().Get("$select") == "" {
		writeGraphError(w, http.StatusBadRequest, "BadRequest")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": m.opts.UserID, "mail": nil, "userPrincipalName": m.opts.Email,
	})
}

func (m *Server) handleCalendarView(w http.ResponseWriter, r *http.Request) {
	m.CountEvents()
	if m.TakeFail("events", w) {
		return
	}
	m.Record(fakeoauth.RequestRecord{Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone()})
	if !m.AccessOK(r) {
		writeGraphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	m.mu.Lock()
	noBox := m.noBox
	evs := append([]Event(nil), m.events...)
	m.mu.Unlock()
	if noBox {
		writeGraphError(w, http.StatusNotFound, "MailboxNotEnabledForRESTAPI")
		return
	}
	if r.Header.Get("Prefer") != `outlook.timezone="UTC"` {
		writeGraphError(w, http.StatusBadRequest, "fake: Relay must ask for UTC")
		return
	}

	q := r.URL.Query()
	start, err1 := time.Parse(time.RFC3339, q.Get("startDateTime"))
	end, err2 := time.Parse(time.RFC3339, q.Get("endDateTime"))
	if err1 != nil || err2 != nil || q.Get("$orderby") != "start/dateTime" {
		writeGraphError(w, http.StatusBadRequest, "ErrorInvalidParameter")
		return
	}
	top := 10 // Graph's default page size
	if raw := q.Get("$top"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeGraphError(w, http.StatusBadRequest, "ErrorInvalidParameter")
			return
		}
		top = n
	}
	offset := 0
	if raw := q.Get("$skiptoken"); raw != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(raw, "fake-"))
		if err != nil || n < 0 {
			writeGraphError(w, http.StatusBadRequest, "ErrorInvalidSkipToken")
			return
		}
		offset = n
	}

	var window []Event
	for _, e := range evs {
		if e.Start.Before(end) && e.End.After(start) {
			window = append(window, e)
		}
	}
	slices.SortStableFunc(window, func(a, b Event) int { return a.Start.Compare(b.Start) })

	body := map[string]any{"value": []any{}}
	if offset < len(window) {
		page := window[offset:min(offset+top, len(window))]
		values := make([]any, 0, len(page))
		for _, e := range page {
			values = append(values, graphEvent(e))
		}
		body["value"] = values
		if next := offset + top; next < len(window) {
			// The real Graph host on purpose: see the package doc.
			nq := url.Values{}
			nq.Set("startDateTime", q.Get("startDateTime"))
			nq.Set("endDateTime", q.Get("endDateTime"))
			nq.Set("$top", strconv.Itoa(top))
			nq.Set("$skiptoken", "fake-"+strconv.Itoa(next))
			body["@odata.nextLink"] = "https://graph.microsoft.com/v1.0/me/calendarView?" + nq.Encode()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// graphEvent renders an event the way Graph does with Prefer UTC:
// wall-clock "2006-01-02T15:04:05.0000000" plus timeZone "UTC".
func graphEvent(e Event) map[string]any {
	const layout = "2006-01-02T15:04:05.0000000"
	start, end := e.Start.UTC(), e.End.UTC()
	if e.AllDay {
		start = time.Date(e.Start.Year(), e.Start.Month(), e.Start.Day(), 0, 0, 0, 0, time.UTC)
		end = time.Date(e.End.Year(), e.End.Month(), e.End.Day(), 0, 0, 0, 0, time.UTC)
	}
	out := map[string]any{
		"id":                    e.ID,
		"subject":               e.Subject,
		"isAllDay":              e.AllDay,
		"isCancelled":           e.Cancelled,
		"start":                 map[string]string{"dateTime": start.Format(layout), "timeZone": "UTC"},
		"end":                   map[string]string{"dateTime": end.Format(layout), "timeZone": "UTC"},
		"originalStartTimeZone": e.TimeZone,
		"location":              map[string]string{"displayName": e.Location},
	}
	if e.OrganizerEmail != "" || e.OrganizerName != "" {
		out["organizer"] = map[string]any{
			"emailAddress": map[string]string{"address": e.OrganizerEmail, "name": e.OrganizerName},
		}
	}
	return out
}

func writeGraphError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}
