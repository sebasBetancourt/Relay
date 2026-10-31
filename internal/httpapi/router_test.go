package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/store"
)

type webhookStub struct{ paths []string }

func (s *webhookStub) Google(w http.ResponseWriter, _ *http.Request) {
	s.paths = append(s.paths, "google")
	w.WriteHeader(http.StatusAccepted)
}

func (s *webhookStub) Microsoft(w http.ResponseWriter, _ *http.Request) {
	s.paths = append(s.paths, "microsoft")
	w.WriteHeader(http.StatusAccepted)
}

func (s *webhookStub) MicrosoftLifecycle(w http.ResponseWriter, _ *http.Request) {
	s.paths = append(s.paths, "lifecycle")
	w.WriteHeader(http.StatusAccepted)
}

func TestWebhooksArePublic(t *testing.T) {
	stub := &webhookStub{}
	router := contractRouter(t, Deps{Log: discardLogger(), Env: apikey.EnvTest, Webhooks: stub})
	for _, path := range []string{"/v1/webhooks/google", "/v1/webhooks/microsoft", "/v1/webhooks/microsoft/lifecycle"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"value":[]}`))
		// What each provider really sends; none of it is an API key.
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Goog-Channel-ID", "channel")
		req.Header.Set("X-Goog-Channel-Token", "secret")
		req.Header.Set("X-Goog-Resource-State", "exists")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s status = %d, want 202", path, rec.Code)
		}
	}
	if got := len(stub.paths); got != 3 || stub.paths[2] != "lifecycle" {
		t.Fatalf("handlers = %v", stub.paths)
	}
}

func TestHealthz(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := contractRouter(t, Deps{Log: log, Env: apikey.EnvTest}) // healthz no usa la DB ni el store

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("expected body ok, got %q", rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("expected X-Request-ID header")
	}
}

func TestRouterV1(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	acme := store.Tenant{ID: "t-1", Name: "Acme", KeyPrefix: key.KeyPrefix}
	lookup := &fakeLookup{tenants: map[string]store.Tenant{key.Hash: acme}}
	router := contractRouter(t, Deps{Log: discardLogger(), Tenants: lookup, Env: apikey.EnvTest})

	do := func(path, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	t.Run("healthz stays public", func(t *testing.T) {
		if rec := do("/healthz", ""); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("me without key", func(t *testing.T) {
		if rec := do("/v1/me", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("unknown v1 route without key hides existence", func(t *testing.T) {
		if rec := do("/v1/nope", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("unknown v1 route with key", func(t *testing.T) {
		if rec := do("/v1/nope", "Bearer "+key.Plaintext); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("me with key", func(t *testing.T) {
		rec := do("/v1/me", "Bearer "+key.Plaintext)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content-type = %q", ct)
		}
		var body map[string]string
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"id": "t-1", "name": "Acme", "key_prefix": key.KeyPrefix}
		for k, v := range want {
			if body[k] != v {
				t.Fatalf("body[%q] = %q, want %q (full %v)", k, body[k], v, body)
			}
		}
		if len(body) != len(want) {
			t.Fatalf("unexpected fields in body: %v", body)
		}
	})
}
