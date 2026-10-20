// Package fakegoogle is a stateful Google stand-in for tests.
// It remembers codes, PKCE challenges, refresh/access tokens and events,
// so a wrong verifier or a foreign refresh fails the way real Google would.
//
// It does not import connect or calendar (avoids import cycles). Callers wire
// its URLs into oauth2.Config / GoogleSource themselves.
package fakegoogle

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	ScopeOpenID           = "openid"
	ScopeCalendarReadonly = "https://www.googleapis.com/auth/calendar.readonly"
	defaultAccessTTL      = time.Hour
)

// Options configures the fake Google.
type Options struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Sub          string        // id_token sub / provider_account_id
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
type RequestRecord struct {
	Path  string
	Form  url.Values
	Query url.Values
}

// Calls are atomic counters of successful-path hits (and attempts).
type Calls struct {
	Exchange int64
	Refresh  int64
	Events   int64
}

// Server is a fake Google OAuth + Calendar API.
type Server struct {
	t         *testing.T
	opts      Options
	srv       *httptest.Server
	mu        sync.Mutex
	codes     map[string]*authCode // code → pending
	refresh   map[string]*refreshTok
	access    map[string]*accessTok
	events    []Event
	failNext  map[string]int // path suffix → status
	last      RequestRecord
	exchangeN atomic.Int64
	refreshN  atomic.Int64
	eventsN   atomic.Int64
}

type authCode struct {
	challenge   string
	redirectURI string
	state       string
	used        bool
}

type refreshTok struct {
	revoked bool
}

type accessTok struct {
	refresh string
	expiry  time.Time
	revoked bool
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
	if opts.AccessTTL <= 0 {
		opts.AccessTTL = defaultAccessTTL
	}
	g := &Server{
		t:        t,
		opts:     opts,
		codes:    make(map[string]*authCode),
		refresh:  make(map[string]*refreshTok),
		access:   make(map[string]*accessTok),
		failNext: make(map[string]int),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/o/oauth2/auth", g.handleAuthPage)
	mux.HandleFunc("/token", g.handleToken)
	mux.HandleFunc("/calendars/primary/events", g.handleEvents)
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *Server) TokenURL() string     { return g.srv.URL + "/token" }
func (g *Server) AuthURL() string      { return g.srv.URL + "/o/oauth2/auth" }
func (g *Server) APIBaseURL() string   { return g.srv.URL }
func (g *Server) Client() *http.Client { return g.srv.Client() }
func (g *Server) Sub() string          { return g.opts.Sub }

func (g *Server) Calls() Calls {
	return Calls{
		Exchange: g.exchangeN.Load(),
		Refresh:  g.refreshN.Load(),
		Events:   g.eventsN.Load(),
	}
}

func (g *Server) LastRequest() RequestRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

// SetEvents replaces the calendar feed.
func (g *Server) SetEvents(evs ...Event) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append([]Event(nil), evs...)
}

// ExpireAccessTokens marks every issued access token as expired.
func (g *Server) ExpireAccessTokens() {
	g.mu.Lock()
	defer g.mu.Unlock()
	past := time.Now().Add(-time.Minute)
	for _, a := range g.access {
		a.expiry = past
	}
}

// RevokeAccess revokes all access tokens (refresh still valid).
func (g *Server) RevokeAccess() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.access {
		a.revoked = true
	}
}

// RevokeAll simulates the user removing app access.
func (g *Server) RevokeAll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.refresh {
		r.revoked = true
	}
	for _, a := range g.access {
		a.revoked = true
	}
}

// FailNext makes the next hit on endpoint return status (once).
// endpoint is "token" or "events".
func (g *Server) FailNext(endpoint string, status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failNext[endpoint] = status
}

// Consent simulates the user accepting: validates the auth URL and returns
// redirect_uri?code=…&state=….
func (g *Server) Consent(authURL string) string {
	g.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		g.t.Errorf("fakegoogle Consent: bad auth URL: %v", err)
		return ""
	}
	q := u.Query()
	if q.Get("client_id") != g.opts.ClientID {
		g.t.Errorf("fakegoogle Consent: client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != g.opts.RedirectURL {
		g.t.Errorf("fakegoogle Consent: redirect_uri = %q want %q", q.Get("redirect_uri"), g.opts.RedirectURL)
	}
	if q.Get("response_type") != "code" {
		g.t.Errorf("fakegoogle Consent: response_type = %q", q.Get("response_type"))
	}
	if q.Get("code_challenge_method") != "S256" {
		g.t.Errorf("fakegoogle Consent: code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	if q.Get("access_type") != "offline" {
		g.t.Errorf("fakegoogle Consent: access_type = %q", q.Get("access_type"))
	}
	scope := q.Get("scope")
	if !strings.Contains(scope, ScopeOpenID) || !strings.Contains(scope, ScopeCalendarReadonly) {
		g.t.Errorf("fakegoogle Consent: scope = %q", scope)
	}
	challenge := q.Get("code_challenge")
	state := q.Get("state")
	if challenge == "" || state == "" {
		g.t.Errorf("fakegoogle Consent: missing challenge or state")
		return ""
	}
	code := randomToken("code")
	g.mu.Lock()
	g.codes[code] = &authCode{
		challenge:   challenge,
		redirectURI: q.Get("redirect_uri"),
		state:       state,
	}
	g.mu.Unlock()

	cb, _ := url.Parse(g.opts.RedirectURL)
	cq := cb.Query()
	cq.Set("code", code)
	cq.Set("state", state)
	cb.RawQuery = cq.Encode()
	return cb.String()
}

// Deny simulates the user rejecting consent.
func (g *Server) Deny(authURL string) string {
	g.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		g.t.Errorf("fakegoogle Deny: bad auth URL: %v", err)
		return ""
	}
	state := u.Query().Get("state")
	cb, _ := url.Parse(g.opts.RedirectURL)
	cq := cb.Query()
	cq.Set("error", "access_denied")
	if state != "" {
		cq.Set("state", state)
	}
	cb.RawQuery = cq.Encode()
	return cb.String()
}

func (g *Server) handleAuthPage(w http.ResponseWriter, r *http.Request) {
	// Tests use Consent/Deny; hitting auth in-browser is unsupported.
	http.Error(w, "use Consent/Deny helpers", http.StatusNotImplemented)
}

func (g *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if g.takeFail("token", w) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	g.mu.Lock()
	g.last = RequestRecord{Path: r.URL.Path, Form: form}
	g.mu.Unlock()

	// Client auth: form or Basic.
	clientID, clientSecret := form.Get("client_id"), form.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		clientID, clientSecret = u, p
	}
	if clientID != g.opts.ClientID || clientSecret != g.opts.ClientSecret {
		writeOAuthError(w, "invalid_client")
		return
	}

	switch form.Get("grant_type") {
	case "authorization_code":
		g.exchangeCode(w, form)
	case "refresh_token":
		g.refreshToken(w, form)
	default:
		writeOAuthError(w, "unsupported_grant_type")
	}
}

func (g *Server) exchangeCode(w http.ResponseWriter, form url.Values) {
	if form.Has("state") {
		writeOAuthError(w, "invalid_request")
		return
	}
	code := form.Get("code")
	verifier := form.Get("code_verifier")
	redirect := form.Get("redirect_uri")

	g.mu.Lock()
	ac, ok := g.codes[code]
	if !ok || ac.used {
		g.mu.Unlock()
		writeOAuthError(w, "invalid_grant")
		return
	}
	if ac.redirectURI != redirect {
		g.mu.Unlock()
		writeOAuthError(w, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	if got != ac.challenge {
		g.mu.Unlock()
		writeOAuthError(w, "invalid_grant")
		return
	}
	ac.used = true
	access, refresh, idTok, exp := g.issueTokensLocked()
	g.mu.Unlock()

	g.exchangeN.Add(1)
	writeTokenJSON(w, access, refresh, idTok, exp, g.opts.AccessTTL)
}

func (g *Server) refreshToken(w http.ResponseWriter, form url.Values) {
	rt := form.Get("refresh_token")
	g.mu.Lock()
	rec, ok := g.refresh[rt]
	if !ok || rec.revoked {
		g.mu.Unlock()
		writeOAuthError(w, "invalid_grant")
		return
	}
	access, _, idTok, exp := g.issueTokensLockedWithRefresh(rt)
	g.mu.Unlock()

	g.refreshN.Add(1)
	writeTokenJSON(w, access, "", idTok, exp, g.opts.AccessTTL)
}

func (g *Server) issueTokensLocked() (access, refresh, idTok string, exp time.Time) {
	refresh = randomToken("rt")
	g.refresh[refresh] = &refreshTok{}
	return g.issueTokensLockedWithRefresh(refresh)
}

func (g *Server) issueTokensLockedWithRefresh(refresh string) (access, _ string, idTok string, exp time.Time) {
	access = randomToken("at")
	exp = time.Now().Add(g.opts.AccessTTL)
	g.access[access] = &accessTok{refresh: refresh, expiry: exp}
	idTok = makeIDToken(g.opts.ClientID, g.opts.Sub, exp)
	return access, refresh, idTok, exp
}

func (g *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	g.eventsN.Add(1) // count every attempt (including 401), for retry asserts
	if g.takeFail("events", w) {
		return
	}
	g.mu.Lock()
	g.last = RequestRecord{Path: r.URL.Path, Query: r.URL.Query()}
	g.mu.Unlock()

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	g.mu.Lock()
	at, ok := g.access[token]
	if !ok || at.revoked || !time.Now().Before(at.expiry) {
		g.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	evs := append([]Event(nil), g.events...)
	g.mu.Unlock()

	q := r.URL.Query()
	if q.Get("singleEvents") != "true" || q.Get("orderBy") != "startTime" {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	if q.Get("timeMin") == "" || q.Get("timeMax") == "" {
		http.Error(w, "missing time window", http.StatusBadRequest)
		return
	}
	timeMin, err1 := time.Parse(time.RFC3339, q.Get("timeMin"))
	timeMax, err2 := time.Parse(time.RFC3339, q.Get("timeMax"))
	if err1 != nil || err2 != nil {
		http.Error(w, "bad time", http.StatusBadRequest)
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
			item["organizer"] = map[string]string{
				"email": e.OrganizerEmail, "displayName": e.OrganizerName,
			}
		}
		items = append(items, item)
		if len(items) >= maxResults {
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"timeZone": "UTC",
		"items":    items,
	})
}

func (g *Server) takeFail(endpoint string, w http.ResponseWriter) bool {
	g.mu.Lock()
	status, ok := g.failNext[endpoint]
	if ok {
		delete(g.failNext, endpoint)
	}
	g.mu.Unlock()
	if !ok {
		return false
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"fail_next"}`))
	return true
}

func eventInWindow(e Event, min, max time.Time) bool {
	var start time.Time
	var err error
	if e.AllDay {
		start, err = time.Parse("2006-01-02", e.Start)
	} else {
		start, err = time.Parse(time.RFC3339, e.Start)
	}
	if err != nil {
		return true // keep malformed events visible in tests
	}
	return !start.Before(min) && start.Before(max)
}

func writeOAuthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func writeTokenJSON(w http.ResponseWriter, access, refresh, idTok string, exp time.Time, ttl time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(ttl.Seconds()),
		"scope":        ScopeOpenID + " " + ScopeCalendarReadonly,
		"id_token":     idTok,
	}
	if refresh != "" {
		body["refresh_token"] = refresh
	}
	_ = exp // expiry encoded via expires_in
	_ = json.NewEncoder(w).Encode(body)
}

func makeIDToken(aud, sub string, exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{
		"iss": "https://accounts.google.com",
		"aud": aud,
		"sub": sub,
		"exp": exp.Unix(),
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func randomToken(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + "-" + base64.RawURLEncoding.EncodeToString(b)
}
