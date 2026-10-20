package fakegoogle_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/testutil/fakegoogle"
)

func authURL(g *fakegoogle.Server, challenge, state string) string {
	u, _ := url.Parse(g.AuthURL())
	q := u.Query()
	q.Set("client_id", "client-test")
	q.Set("redirect_uri", "http://localhost:8080/v1/oauth/google/callback")
	q.Set("response_type", "code")
	q.Set("scope", fakegoogle.ScopeOpenID+" "+fakegoogle.ScopeCalendarReadonly)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("access_type", "offline")
	q.Set("state", state)
	u.RawQuery = q.Encode()
	return u.String()
}

func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func exchange(t *testing.T, g *fakegoogle.Server, code, verifier string) (map[string]any, int) {
	t.Helper()
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", "http://localhost:8080/v1/oauth/google/callback")
	form.Set("client_id", "client-test")
	form.Set("client_secret", "secret-test")
	res, err := g.Client().Post(g.TokenURL(), "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return out, res.StatusCode
}

func TestCodeSingleUseAndPKCE(t *testing.T) {
	g := fakegoogle.New(t, fakegoogle.Options{})
	verifier := "verifier-abc-123456789012345678901"
	cb := g.Consent(authURL(g, challengeOf(verifier), "st1"))
	u, _ := url.Parse(cb)
	code := u.Query().Get("code")

	tok, status := exchange(t, g, code, verifier)
	if status != 200 || tok["access_token"] == nil || tok["refresh_token"] == nil {
		t.Fatalf("status=%d tok=%v", status, tok)
	}

	_, status = exchange(t, g, code, verifier)
	if status != 400 {
		t.Fatalf("reuse status=%d", status)
	}

	cb2 := g.Consent(authURL(g, challengeOf(verifier), "st2"))
	code2 := mustQuery(t, cb2, "code")
	_, status = exchange(t, g, code2, "wrong-verifier")
	if status != 400 {
		t.Fatalf("bad pkce status=%d", status)
	}
}

func TestRevokedRefreshAndExpiredAccess(t *testing.T) {
	g := fakegoogle.New(t, fakegoogle.Options{AccessTTL: time.Hour})
	verifier := "verifier-xyz-123456789012345678901"
	cb := g.Consent(authURL(g, challengeOf(verifier), "st"))
	tok, _ := exchange(t, g, mustQuery(t, cb, "code"), verifier)
	refresh := tok["refresh_token"].(string)
	access := tok["access_token"].(string)

	g.SetEvents(fakegoogle.Event{
		ID: "e1", Summary: "Hi",
		Start: time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		End:   time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339),
	})

	req, _ := http.NewRequest(http.MethodGet, g.APIBaseURL()+"/calendars/primary/events?"+url.Values{
		"singleEvents": {"true"}, "orderBy": {"startTime"},
		"timeMin": {time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)},
		"timeMax": {time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)},
	}.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+access)
	res, err := g.Client().Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("events: %v status=%d", err, res.StatusCode)
	}
	_ = res.Body.Close()

	g.ExpireAccessTokens()
	res, _ = g.Client().Do(req)
	if res.StatusCode != 401 {
		t.Fatalf("expired access status=%d", res.StatusCode)
	}
	_ = res.Body.Close()

	g.RevokeAll()
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", "client-test")
	form.Set("client_secret", "secret-test")
	res, err = g.Client().Post(g.TokenURL(), "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 400 {
		t.Fatalf("revoked refresh status=%d", res.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body["error"] != "invalid_grant" {
		t.Fatalf("body=%v", body)
	}
}

func mustQuery(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	v := u.Query().Get(key)
	if v == "" {
		t.Fatalf("missing %s in %s", key, raw)
	}
	return v
}
