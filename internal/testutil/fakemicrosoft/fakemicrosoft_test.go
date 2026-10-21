package fakemicrosoft

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const verifier = "verifier-0123456789-0123456789-0123456789"

func challenge() string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authURL(m *Server) string {
	q := url.Values{
		"client_id": {m.opts.ClientID}, "redirect_uri": {m.opts.RedirectURL},
		"response_type": {"code"}, "state": {"st"},
		"scope":          {"openid email offline_access User.Read Calendars.Read"},
		"code_challenge": {challenge()}, "code_challenge_method": {"S256"},
		"prompt": {"select_account"},
	}
	return m.AuthURL() + "?" + q.Encode()
}

func codeFrom(t *testing.T, callback string) string {
	t.Helper()
	u, _ := url.Parse(callback)
	return u.Query().Get("code")
}

// tokenRequest posts a token request with the secret in the body.
func tokenRequest(t *testing.T, m *Server, form url.Values, basic bool) (int, map[string]any) {
	t.Helper()
	if !basic {
		form.Set("client_id", m.opts.ClientID)
		form.Set("client_secret", m.opts.ClientSecret)
	}
	req, _ := http.NewRequest(http.MethodPost, m.TokenURL(), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic {
		req.SetBasicAuth(m.opts.ClientID, m.opts.ClientSecret)
	}
	resp, err := m.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func exchange(t *testing.T, m *Server) (access, refresh string) {
	t.Helper()
	code := codeFrom(t, m.Consent(authURL(m)))
	status, body := tokenRequest(t, m, url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {m.opts.RedirectURL}, "code_verifier": {verifier},
	}, false)
	if status != 200 {
		t.Fatalf("exchange: %d %v", status, body)
	}
	return body["access_token"].(string), body["refresh_token"].(string)
}

func TestRefreshRotatesAndInvalidatesTheOldToken(t *testing.T) {
	m := New(t, Options{})
	_, r0 := exchange(t, m)

	status, body := tokenRequest(t, m, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r0}}, false)
	if status != 200 {
		t.Fatalf("refresh: %d %v", status, body)
	}
	r1, _ := body["refresh_token"].(string)
	if r1 == "" || r1 == r0 {
		t.Fatalf("refresh must rotate: got %q", r1)
	}
	if m.RefreshTokenValid(r0) || !m.RefreshTokenValid(r1) {
		t.Fatal("old refresh must die, new one must work")
	}
	if status, body := tokenRequest(t, m, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r0}}, false); status != 400 || body["error"] != "invalid_grant" {
		t.Fatalf("reusing the old refresh: %d %v, want invalid_grant", status, body)
	}
}

func TestTokenEndpointIsStrict(t *testing.T) {
	m := New(t, Options{})
	code := codeFrom(t, m.Consent(authURL(m)))

	// Wrong PKCE verifier.
	if status, body := tokenRequest(t, m, url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {m.opts.RedirectURL}, "code_verifier": {"wrong"},
	}, false); status != 400 || body["error"] != "invalid_grant" {
		t.Fatalf("bad verifier: %d %v", status, body)
	}
	// Secret via HTTP Basic is rejected (Relay sends it in the body).
	if status, body := tokenRequest(t, m, url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {m.opts.RedirectURL}, "code_verifier": {verifier},
	}, true); status != 400 || body["error"] != "invalid_client" {
		t.Fatalf("basic auth: %d %v", status, body)
	}
	// One forced refresh failure.
	_, r := exchange(t, m)
	m.FailNextRefresh("interaction_required")
	if status, body := tokenRequest(t, m, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {r}}, false); status != 400 || body["error"] != "interaction_required" {
		t.Fatalf("FailNextRefresh: %d %v", status, body)
	}
}

func getJSON(t *testing.T, m *Server, path, access string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, m.GraphBaseURL()+path, nil)
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := m.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestMe(t *testing.T) {
	m := New(t, Options{UserID: "u-1", Email: "ana@outlook.com"})
	access, _ := exchange(t, m)

	if status, _ := getJSON(t, m, "/v1.0/me?$select=id", "", nil); status != 401 {
		t.Fatalf("no token: %d", status)
	}
	status, body := getJSON(t, m, "/v1.0/me?$select=id,mail,userPrincipalName", access, nil)
	if status != 200 || body["id"] != "u-1" || body["mail"] != nil || body["userPrincipalName"] != "ana@outlook.com" {
		t.Fatalf("me: %d %v", status, body)
	}
}

func TestCalendarViewPaging(t *testing.T) {
	m := New(t, Options{})
	access, _ := exchange(t, m)
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	var evs []Event
	for i := range 5 {
		evs = append(evs, Event{ID: string(rune('a' + i)), Subject: "e", Start: base.Add(time.Duration(i) * time.Hour), End: base.Add(time.Duration(i)*time.Hour + 30*time.Minute)})
	}
	m.SetEvents(evs...)
	utc := map[string]string{"Prefer": `outlook.timezone="UTC"`}
	q := "?startDateTime=2026-10-20T00:00:00Z&endDateTime=2026-10-21T00:00:00Z&$orderby=start/dateTime&$top=2"

	status, body := getJSON(t, m, "/v1.0/me/calendarView"+q, access, utc)
	if status != 200 || len(body["value"].([]any)) != 2 {
		t.Fatalf("page 1: %d %v", status, body)
	}
	next, _ := url.Parse(body["@odata.nextLink"].(string))
	if next.Host != "graph.microsoft.com" || next.Query().Get("$skiptoken") != "fake-2" {
		t.Fatalf("nextLink = %s, want the real Graph host and a skiptoken", next)
	}

	status, body = getJSON(t, m, "/v1.0/me/calendarView"+q+"&$skiptoken=fake-4", access, utc)
	if status != 200 || len(body["value"].([]any)) != 1 || body["@odata.nextLink"] != nil {
		t.Fatalf("last page: %d %v", status, body)
	}

	if status, _ := getJSON(t, m, "/v1.0/me/calendarView"+q, access, nil); status != 400 {
		t.Fatalf("without Prefer UTC: %d, want 400", status)
	}
	m.MailboxNotEnabled(true)
	status, body = getJSON(t, m, "/v1.0/me/calendarView"+q, access, utc)
	if code := body["error"].(map[string]any)["code"]; status != 404 || code != "MailboxNotEnabledForRESTAPI" {
		t.Fatalf("no mailbox: %d %v", status, body)
	}
}
