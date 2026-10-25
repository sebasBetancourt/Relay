// Package fakeoauth is the provider-neutral core of Relay's stateful fake
// OAuth providers (fakegoogle, fakemicrosoft). It remembers what it issued
// (one-time codes bound to PKCE challenges, refresh and access tokens) so a
// wrong verifier, a reused code, a revoked or rotated-out refresh token, or
// an expired access token fails the way a real provider would.
//
// It does not import Relay's oauth or calendar packages (no import cycles):
// provider fakes wire its handlers into their own routes.
package fakeoauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Config is what differs between providers.
type Config struct {
	Name         string // used in test failure messages, e.g. "fakegoogle"
	ClientID     string
	ClientSecret string
	RedirectURL  string
	AccessTTL    time.Duration

	// RotateRefresh: every refresh returns a new refresh token and
	// invalidates the one it received (Microsoft, made strict on purpose).
	RotateRefresh bool
	// SecretInBodyOnly rejects HTTP Basic client authentication.
	SecretInBodyOnly bool

	// ValidateAuthURL checks provider-specific consent parameters
	// (scopes, access_type, prompt…); report problems with t.Errorf.
	ValidateAuthURL func(t *testing.T, q url.Values)
	// TokenExtras adds provider fields (scope, id_token) to token responses.
	TokenExtras func(accessExpiry time.Time) map[string]any
}

// RequestRecord is the last observed request (for asserts).
type RequestRecord struct {
	Path   string
	Form   url.Values
	Query  url.Values
	Header http.Header
}

// Calls counts token-endpoint successes and calendar API attempts.
type Calls struct {
	Exchange int64
	Refresh  int64
	Events   int64 // every attempt, including 401s (for retry asserts)
}

// Core holds the provider state.
type Core struct {
	t   *testing.T
	cfg Config

	mu          sync.Mutex
	codes       map[string]*authCode
	refresh     map[string]*refreshTok
	access      map[string]*accessTok
	failNext    map[string]int // "token" | "events" → HTTP status (once)
	refreshFail string         // next refresh_token grant fails with this OAuth error code
	refreshWait time.Duration  // simulated provider latency on refresh
	last        RequestRecord

	exchangeN atomic.Int64
	refreshN  atomic.Int64
	eventsN   atomic.Int64
}

type authCode struct {
	challenge   string
	redirectURI string
	used        bool
}

type refreshTok struct {
	revoked bool
}

type accessTok struct {
	expiry  time.Time
	revoked bool
}

// New builds the core. Defaults: AccessTTL 1h.
func New(t *testing.T, cfg Config) *Core {
	t.Helper()
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = time.Hour
	}
	if cfg.Name == "" {
		cfg.Name = "fakeoauth"
	}
	return &Core{
		t:        t,
		cfg:      cfg,
		codes:    make(map[string]*authCode),
		refresh:  make(map[string]*refreshTok),
		access:   make(map[string]*accessTok),
		failNext: make(map[string]int),
	}
}

// Calls returns the counters.
func (c *Core) Calls() Calls {
	return Calls{Exchange: c.exchangeN.Load(), Refresh: c.refreshN.Load(), Events: c.eventsN.Load()}
}

// CountEvents records one calendar API attempt.
func (c *Core) CountEvents() { c.eventsN.Add(1) }

// LastRequest returns the last recorded request.
func (c *Core) LastRequest() RequestRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Record stores r as the last request.
func (c *Core) Record(r RequestRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = r
}

// ExpireAccessTokens marks every issued access token as expired.
func (c *Core) ExpireAccessTokens() {
	c.mu.Lock()
	defer c.mu.Unlock()
	past := time.Now().Add(-time.Minute)
	for _, a := range c.access {
		a.expiry = past
	}
}

// RevokeAccess revokes all access tokens; refresh tokens keep working.
func (c *Core) RevokeAccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.access {
		a.revoked = true
	}
}

// RevokeAll simulates the user removing the app: nothing works anymore.
func (c *Core) RevokeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.refresh {
		r.revoked = true
	}
	for _, a := range c.access {
		a.revoked = true
	}
}

// RevokeToken revokes one refresh or access token (RFC 7009 semantics:
// revoking a refresh token also kills the access tokens). Reports whether
// the token was known.
func (c *Core) RevokeToken(token string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.refresh[token]; ok {
		r.revoked = true
		for _, a := range c.access {
			a.revoked = true
		}
		return true
	}
	if a, ok := c.access[token]; ok {
		a.revoked = true
		return true
	}
	return false
}

// RefreshTokenValid reports whether token would be accepted by a refresh.
func (c *Core) RefreshTokenValid(token string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.refresh[token]
	return ok && !r.revoked
}

// FailNext makes the next hit on endpoint ("token" or "events") answer
// status, once.
func (c *Core) FailNext(endpoint string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failNext[endpoint] = status
}

// FailNextRefresh makes the next refresh_token grant fail with the OAuth
// error code (e.g. "interaction_required"), once.
func (c *Core) FailNextRefresh(code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshFail = code
}

// SetRefreshLatency makes every refresh_token grant take d, like a real
// provider. Concurrency tests need it: with an instant fake, one caller
// finishes before the next starts and races never happen.
func (c *Core) SetRefreshLatency(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshWait = d
}

// TakeFail writes the pending failure for endpoint, if any.
func (c *Core) TakeFail(endpoint string, w http.ResponseWriter) bool {
	c.mu.Lock()
	status, ok := c.failNext[endpoint]
	if ok {
		delete(c.failNext, endpoint)
	}
	c.mu.Unlock()
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"fail_next"}`))
	return true
}

// Consent simulates the user accepting: it validates the consent URL and
// returns redirect_uri?code=…&state=… with a one-time code bound to the
// PKCE challenge.
func (c *Core) Consent(authURL string) string {
	c.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		c.t.Errorf("%s Consent: bad auth URL: %v", c.cfg.Name, err)
		return ""
	}
	q := u.Query()
	if q.Get("client_id") != c.cfg.ClientID {
		c.t.Errorf("%s Consent: client_id = %q", c.cfg.Name, q.Get("client_id"))
	}
	if q.Get("redirect_uri") != c.cfg.RedirectURL {
		c.t.Errorf("%s Consent: redirect_uri = %q, want %q", c.cfg.Name, q.Get("redirect_uri"), c.cfg.RedirectURL)
	}
	if q.Get("response_type") != "code" {
		c.t.Errorf("%s Consent: response_type = %q", c.cfg.Name, q.Get("response_type"))
	}
	if q.Get("code_challenge_method") != "S256" {
		c.t.Errorf("%s Consent: code_challenge_method = %q", c.cfg.Name, q.Get("code_challenge_method"))
	}
	if c.cfg.ValidateAuthURL != nil {
		c.cfg.ValidateAuthURL(c.t, q)
	}
	challenge, state := q.Get("code_challenge"), q.Get("state")
	if challenge == "" || state == "" {
		c.t.Errorf("%s Consent: missing code_challenge or state", c.cfg.Name)
		return ""
	}

	code := RandomToken("code")
	c.mu.Lock()
	c.codes[code] = &authCode{challenge: challenge, redirectURI: q.Get("redirect_uri")}
	c.mu.Unlock()
	return c.redirect(url.Values{"code": {code}, "state": {state}})
}

// Deny simulates the user rejecting consent.
func (c *Core) Deny(authURL string) string {
	c.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		c.t.Errorf("%s Deny: bad auth URL: %v", c.cfg.Name, err)
		return ""
	}
	v := url.Values{"error": {"access_denied"}}
	if state := u.Query().Get("state"); state != "" {
		v.Set("state", state)
	}
	return c.redirect(v)
}

func (c *Core) redirect(v url.Values) string {
	cb, _ := url.Parse(c.cfg.RedirectURL)
	q := cb.Query()
	for k, vals := range v {
		q[k] = vals
	}
	cb.RawQuery = q.Encode()
	return cb.String()
}

// AccessOK reports whether r carries a valid Bearer access token.
// IssueAccess inserts a live access token. Tests that call a provider API
// without going through the token endpoint use it.
func (c *Core) IssueAccess(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.access[token] = &accessTok{expiry: time.Now().Add(c.cfg.AccessTTL)}
}

func (c *Core) AccessOK(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.access[token]
	return ok && !a.revoked && time.Now().Before(a.expiry)
}

// HandleToken is the token endpoint: authorization_code (with PKCE) and
// refresh_token grants.
func (c *Core) HandleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if c.TakeFail("token", w) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	c.Record(RequestRecord{Path: r.URL.Path, Form: form, Header: r.Header.Clone()})

	clientID, clientSecret := form.Get("client_id"), form.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		if c.cfg.SecretInBodyOnly {
			WriteOAuthError(w, "invalid_client")
			return
		}
		clientID, clientSecret = u, p
	}
	if clientID != c.cfg.ClientID || clientSecret != c.cfg.ClientSecret {
		WriteOAuthError(w, "invalid_client")
		return
	}

	switch form.Get("grant_type") {
	case "authorization_code":
		c.exchangeCode(w, form)
	case "refresh_token":
		c.refreshGrant(w, form)
	default:
		WriteOAuthError(w, "unsupported_grant_type")
	}
}

func (c *Core) exchangeCode(w http.ResponseWriter, form url.Values) {
	if form.Has("state") {
		WriteOAuthError(w, "invalid_request") // state never goes to the token endpoint
		return
	}
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))

	c.mu.Lock()
	ac, ok := c.codes[form.Get("code")]
	if !ok || ac.used || ac.redirectURI != form.Get("redirect_uri") ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != ac.challenge {
		c.mu.Unlock()
		WriteOAuthError(w, "invalid_grant")
		return
	}
	ac.used = true
	refresh := RandomToken("rt")
	c.refresh[refresh] = &refreshTok{}
	access, exp := c.issueAccessLocked()
	c.mu.Unlock()

	c.exchangeN.Add(1)
	c.writeTokens(w, access, refresh, exp)
}

func (c *Core) refreshGrant(w http.ResponseWriter, form url.Values) {
	old := form.Get("refresh_token")

	c.mu.Lock()
	wait := c.refreshWait
	c.mu.Unlock()
	time.Sleep(wait) // outside the lock: concurrent refreshes overlap

	c.mu.Lock()
	if code := c.refreshFail; code != "" {
		c.refreshFail = ""
		c.mu.Unlock()
		WriteOAuthError(w, code)
		return
	}
	rec, ok := c.refresh[old]
	if !ok || rec.revoked {
		c.mu.Unlock()
		WriteOAuthError(w, "invalid_grant")
		return
	}
	newRefresh := ""
	if c.cfg.RotateRefresh {
		rec.revoked = true // the token just used is dead from now on
		newRefresh = RandomToken("rt")
		c.refresh[newRefresh] = &refreshTok{}
	}
	access, exp := c.issueAccessLocked()
	c.mu.Unlock()

	c.refreshN.Add(1)
	c.writeTokens(w, access, newRefresh, exp)
}

func (c *Core) issueAccessLocked() (string, time.Time) {
	access := RandomToken("at")
	exp := time.Now().Add(c.cfg.AccessTTL)
	c.access[access] = &accessTok{expiry: exp}
	return access, exp
}

func (c *Core) writeTokens(w http.ResponseWriter, access, refresh string, exp time.Time) {
	body := map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(c.cfg.AccessTTL.Seconds()),
	}
	if refresh != "" {
		body["refresh_token"] = refresh
	}
	if c.cfg.TokenExtras != nil {
		for k, v := range c.cfg.TokenExtras(exp) {
			body[k] = v
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// WriteOAuthError writes a 400 {"error": code} (RFC 6749 §5.2).
func WriteOAuthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// RandomToken returns prefix-<random>.
func RandomToken(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + "-" + base64.RawURLEncoding.EncodeToString(b)
}
