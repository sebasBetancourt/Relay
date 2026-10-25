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
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	opts      Options
	srv       *httptest.Server
	mu        sync.Mutex
	events    []Event
	noBox     bool
	meN       atomic.Int64
	subs      map[string]GraphSub
	subN      atomic.Int64
	patchN    atomic.Int64
	deleteN   atomic.Int64
	lastSub   GraphSub
	deltaSnap map[string]Event
	deltaTok  string
	deltaGone bool
	lastDelta string
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
	m := &Server{opts: opts, subs: map[string]GraphSub{}}
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
	mux.HandleFunc("GET /v1.0/me/calendarView/delta", m.handleDelta)
	mux.HandleFunc("POST /v1.0/subscriptions", m.handleCreateSub)
	mux.HandleFunc("PATCH /v1.0/subscriptions/{id}", m.handlePatchSub)
	mux.HandleFunc("DELETE /v1.0/subscriptions/{id}", m.handleDeleteSub)
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
		"changeKey":             changeKey(e),
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

func changeKey(e Event) string {
	return fmt.Sprintf("%s|%s|%s|%t", e.ID, e.Subject, e.Start.UTC().Format(time.RFC3339Nano), e.Cancelled)
}

// ExpireDelta makes the next incremental delta answer 410.
func (m *Server) ExpireDelta() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deltaGone = true
}

// LastDeltaURL is the path and query of the last delta request.
func (m *Server) LastDeltaURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastDelta
}

func (m *Server) handleDelta(w http.ResponseWriter, r *http.Request) {
	m.CountEvents()
	if m.TakeFail("delta", w) {
		return
	}
	if !m.AccessOK(r) {
		writeGraphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	q := r.URL.Query()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastDelta = r.URL.RequestURI()

	if tok := q.Get("$deltatoken"); tok != "" {
		if m.deltaGone || tok != m.deltaTok {
			m.deltaGone = false
			writeGraphError(w, http.StatusGone, "SyncStateNotFound")
			return
		}
		m.writeDelta(w, m.diffDelta())
		return
	}
	start, err1 := time.Parse(time.RFC3339, q.Get("startDateTime"))
	end, err2 := time.Parse(time.RFC3339, q.Get("endDateTime"))
	if err1 != nil || err2 != nil {
		writeGraphError(w, http.StatusBadRequest, "ErrorInvalidParameter")
		return
	}
	var window []Event
	for _, e := range m.events {
		if e.Cancelled {
			continue
		}
		if e.Start.Before(end) && e.End.After(start) {
			window = append(window, e)
		}
	}
	m.writeDelta(w, window)
}

func (m *Server) diffDelta() []Event {
	live := map[string]Event{}
	for _, e := range m.events {
		if !e.Cancelled {
			live[e.ID] = e
		}
	}
	var out []Event
	for id, old := range m.deltaSnap {
		cur, ok := live[id]
		if !ok {
			old.Cancelled = true
			out = append(out, old)
			continue
		}
		if changeKey(cur) != changeKey(old) {
			out = append(out, cur)
		}
		delete(live, id)
	}
	for _, e := range live {
		out = append(out, e)
	}
	return out
}

func (m *Server) writeDelta(w http.ResponseWriter, items []Event) {
	values := make([]any, 0, len(items))
	for _, e := range items {
		if e.Cancelled {
			values = append(values, map[string]any{
				"id":       e.ID,
				"@removed": map[string]string{"reason": "deleted"},
			})
			continue
		}
		values = append(values, graphEvent(e))
	}
	token := fmt.Sprintf("delta-%d", time.Now().UnixNano())
	m.deltaTok = token
	m.deltaGone = false
	m.deltaSnap = map[string]Event{}
	for _, e := range m.events {
		if !e.Cancelled {
			m.deltaSnap[e.ID] = e
		}
	}
	link := m.srv.URL + "/v1.0/me/calendarView/delta?$deltatoken=" + token
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"value":            values,
		"@odata.deltaLink": link,
	})
}

// GraphSub is a subscription the fake accepted.
type GraphSub struct {
	ID, ClientState, NotificationURL, LifecycleURL, Resource string
	Expiration                                               time.Time
}

// Subscriptions counts successful creates.
func (m *Server) Subscriptions() int64 { return m.subN.Load() }

// Patches counts successful subscription updates.
func (m *Server) Patches() int64 { return m.patchN.Load() }

// Deletes counts successful subscription deletes.
func (m *Server) Deletes() int64 { return m.deleteN.Load() }

// LastSubscription is the most recently created subscription.
func (m *Server) LastSubscription() GraphSub {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastSub
}

func (m *Server) handleCreateSub(w http.ResponseWriter, r *http.Request) {
	if m.TakeFail("subscriptions", w) {
		return
	}
	if !m.AccessOK(r) {
		writeGraphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	var body struct {
		ChangeType               string `json:"changeType"`
		NotificationURL          string `json:"notificationUrl"`
		LifecycleNotificationURL string `json:"lifecycleNotificationUrl"`
		Resource                 string `json:"resource"`
		ExpirationDateTime       string `json:"expirationDateTime"`
		ClientState              string `json:"clientState"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeGraphError(w, http.StatusBadRequest, "BadRequest")
		return
	}
	exp, err := time.Parse(time.RFC3339, body.ExpirationDateTime)
	if err != nil || body.Resource != "me/events" || body.ClientState == "" ||
		!strings.Contains(body.ChangeType, "created") ||
		!strings.HasSuffix(body.NotificationURL, "/v1/webhooks/microsoft") ||
		!strings.HasSuffix(body.LifecycleNotificationURL, "/v1/webhooks/microsoft/lifecycle") {
		writeGraphError(w, http.StatusBadRequest, "BadRequest")
		return
	}
	if exp.After(time.Now().Add(10080*time.Minute + time.Minute)) {
		writeGraphError(w, http.StatusBadRequest, "ExpirationTooFar")
		return
	}
	if err := m.handshake(body.NotificationURL); err != nil {
		writeGraphError(w, http.StatusBadRequest, "ValidationError")
		return
	}
	id := "sub-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	sub := GraphSub{
		ID: id, ClientState: body.ClientState, NotificationURL: body.NotificationURL,
		LifecycleURL: body.LifecycleNotificationURL, Resource: body.Resource, Expiration: exp.UTC(),
	}
	m.mu.Lock()
	m.subs[id] = sub
	m.lastSub = sub
	m.mu.Unlock()
	m.subN.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": id, "resource": body.Resource, "expirationDateTime": exp.UTC().Format(time.RFC3339),
	})
}

func (m *Server) handshake(notificationURL string) error {
	token := "valid token"
	u, err := url.Parse(notificationURL)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("validationToken", token)
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK || string(body) != token {
		return fmt.Errorf("validation handshake: status %d body %q", resp.StatusCode, body)
	}
	return nil
}

func (m *Server) handlePatchSub(w http.ResponseWriter, r *http.Request) {
	if m.TakeFail("subscriptions", w) {
		return
	}
	if !m.AccessOK(r) {
		writeGraphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	id := r.PathValue("id")
	m.mu.Lock()
	sub, ok := m.subs[id]
	m.mu.Unlock()
	if !ok {
		writeGraphError(w, http.StatusNotFound, "ResourceNotFound")
		return
	}
	var body struct {
		ExpirationDateTime string `json:"expirationDateTime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeGraphError(w, http.StatusBadRequest, "BadRequest")
		return
	}
	exp, err := time.Parse(time.RFC3339, body.ExpirationDateTime)
	if err != nil || exp.After(time.Now().Add(10080*time.Minute+time.Minute)) {
		writeGraphError(w, http.StatusBadRequest, "ExpirationTooFar")
		return
	}
	sub.Expiration = exp.UTC()
	m.mu.Lock()
	m.subs[id] = sub
	m.mu.Unlock()
	m.patchN.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "expirationDateTime": exp.UTC().Format(time.RFC3339)})
}

func (m *Server) handleDeleteSub(w http.ResponseWriter, r *http.Request) {
	if m.TakeFail("subscriptions", w) {
		return
	}
	if !m.AccessOK(r) {
		writeGraphError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
		return
	}
	id := r.PathValue("id")
	m.mu.Lock()
	_, ok := m.subs[id]
	if ok {
		delete(m.subs, id)
	}
	m.mu.Unlock()
	if !ok {
		writeGraphError(w, http.StatusNotFound, "ResourceNotFound")
		return
	}
	m.deleteN.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

func writeGraphError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}
