package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

type fakeCallback struct {
	res   oauth.Result
	err   error
	calls int
	last  struct {
		provider oauth.Provider
		state    string
		code     string
		perr     string
	}
}

func (f *fakeCallback) Finish(_ context.Context, provider oauth.Provider, state, code, providerErr string) (oauth.Result, error) {
	f.calls++
	f.last.provider, f.last.state, f.last.code, f.last.perr = provider, state, code, providerErr
	return f.res, f.err
}

func TestCallbackPublicAndMeStillProtected(t *testing.T) {
	cb := &fakeCallback{res: oauth.Result{AccountID: "a1", Provider: oauth.ProviderGoogle}}
	router := NewRouter(Deps{
		Log:      discardLogger(),
		Tenants:  &fakeLookup{},
		Env:      apikey.EnvTest,
		Callback: cb,
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/oauth/google/callback?state=s&code=c", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("callback without API key: status %d body %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/me without key: status %d", rec.Code)
	}
}

func TestCallbackStatusTable(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		finishErr  error
		wantStatus int
		wantError  string
	}{
		{"missing state", "code=c", nil, 400, "invalid_request"},
		{"missing code and error", "state=s", nil, 400, "invalid_request"},
		{"unsupported", "state=s&code=c", oauth.ErrUnsupportedProvider, 400, "unsupported_provider"},
		{"invalid state", "state=s&code=c", store.ErrNotFound, 400, "invalid_state"},
		{"expired state", "state=s&code=c", store.ErrStateExpired, 400, "invalid_state"},
		{"access denied", "state=s&error=access_denied", oauth.ErrConsentDenied, 400, "access_denied"},
		{"exchange failed", "state=s&code=c", oauth.ErrExchangeRejected, 400, "exchange_failed"},
		{"insufficient scope", "state=s&code=c", oauth.ErrInsufficientScope, 400, "insufficient_scope"},
		{"bad id token", "state=s&code=c", oauth.ErrInvalidIDToken, 502, "provider_error"},
		{"google down", "state=s&code=c", oauth.ErrProviderUnavailable, 502, "provider_unavailable"},
		{"account taken", "state=s&code=c", store.ErrAccountTaken, 409, "account_already_connected"},
		{"internal", "state=s&code=c", errors.New("db boom"), 500, "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cb := &fakeCallback{err: tt.finishErr, res: oauth.Result{AccountID: "a1", Provider: oauth.ProviderGoogle}}
			router := NewRouter(Deps{Log: discardLogger(), Callback: cb, Env: apikey.EnvTest, Tenants: &fakeLookup{}})

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/oauth/google/callback?"+tt.query, nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var body errorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Error != tt.wantError {
				t.Fatalf("error = %q, want %q", body.Error, tt.wantError)
			}
			raw := rec.Body.String()
			if strings.Contains(raw, "access_token") || strings.Contains(raw, "refresh") {
				t.Fatalf("body leaked tokens: %s", raw)
			}
		})
	}
}

func TestCallbackSuccess(t *testing.T) {
	cb := &fakeCallback{res: oauth.Result{AccountID: "acct-9", Provider: oauth.ProviderGoogle}}
	router := NewRouter(Deps{Log: discardLogger(), Callback: cb, Env: apikey.EnvTest, Tenants: &fakeLookup{}})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/oauth/google/callback?state=st&code=cd", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "connected" || body["provider"] != "google" || body["account_id"] != "acct-9" {
		t.Fatalf("body = %#v", body)
	}
	if _, ok := body["access_token"]; ok {
		t.Fatal("must not include tokens")
	}
	if cb.last.state != "st" || cb.last.code != "cd" {
		t.Fatalf("finish args %+v", cb.last)
	}
}
