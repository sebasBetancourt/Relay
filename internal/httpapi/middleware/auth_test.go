package middleware

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/store"
)

// fakeLookup stands in for the store: known hashes return a tenant,
// revoked hashes ErrRevoked, err (if set) fails every call.
type fakeLookup struct {
	tenants map[string]store.Tenant
	revoked map[string]bool
	err     error
	calls   int
}

func (f *fakeLookup) ByAPIKeyHash(_ context.Context, hash string) (store.Tenant, error) {
	f.calls++
	if f.err != nil {
		return store.Tenant{}, f.err
	}
	if f.revoked[hash] {
		return store.Tenant{}, store.ErrRevoked
	}
	t, ok := f.tenants[hash]
	if !ok {
		return store.Tenant{}, store.ErrNotFound
	}
	return t, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRequireAPIKey(t *testing.T) {
	good := apikey.Generate(apikey.EnvTest)
	revoked := apikey.Generate(apikey.EnvTest)
	unknown := apikey.Generate(apikey.EnvTest)
	live := apikey.Generate(apikey.EnvLive)
	acme := store.Tenant{ID: "t-1", Name: "Acme", KeyPrefix: good.KeyPrefix}

	// Flip one char of the checksum: right shape, wrong CRC.
	last := good.Plaintext[len(good.Plaintext)-1]
	flipped := byte('A')
	if last == 'A' {
		flipped = 'B'
	}
	badChecksum := good.Plaintext[:len(good.Plaintext)-1] + string(flipped)

	tests := []struct {
		name       string
		header     string
		lookupErr  error
		wantStatus int
		wantLookup bool // whether the DB lookup should be reached
	}{
		{"no header", "", nil, http.StatusUnauthorized, false},
		{"basic scheme", "Basic " + good.Plaintext, nil, http.StatusUnauthorized, false},
		{"bearer without token", "Bearer ", nil, http.StatusUnauthorized, false},
		{"not a relay key", "Bearer sk_live_abc", nil, http.StatusUnauthorized, false},
		{"bad checksum", "Bearer " + badChecksum, nil, http.StatusUnauthorized, false},
		{"live key on test server", "Bearer " + live.Plaintext, nil, http.StatusUnauthorized, false},
		{"unknown key", "Bearer " + unknown.Plaintext, nil, http.StatusUnauthorized, true},
		{"revoked key", "Bearer " + revoked.Plaintext, nil, http.StatusUnauthorized, true},
		{"db failure", "Bearer " + good.Plaintext, errors.New("connection refused"), http.StatusInternalServerError, true},
		{"valid key", "Bearer " + good.Plaintext, nil, http.StatusOK, true},
		{"lowercase scheme", "bearer " + good.Plaintext, nil, http.StatusOK, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := &fakeLookup{
				tenants: map[string]store.Tenant{good.Hash: acme},
				revoked: map[string]bool{revoked.Hash: true},
				err:     tt.lookupErr,
			}
			var gotTenant store.Tenant
			var nextCalled bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				gotTenant, _ = TenantFromContext(r.Context())
			})

			req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			RequireAPIKey(discardLogger(), lookup, apikey.EnvTest, nil)(next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if (lookup.calls > 0) != tt.wantLookup {
				t.Fatalf("lookup calls = %d, want reached=%v", lookup.calls, tt.wantLookup)
			}
			if nextCalled != (tt.wantStatus == http.StatusOK) {
				t.Fatalf("next called = %v with status %d", nextCalled, rec.Code)
			}

			switch tt.wantStatus {
			case http.StatusOK:
				if gotTenant != acme {
					t.Fatalf("tenant in context = %+v, want %+v", gotTenant, acme)
				}
			case http.StatusUnauthorized:
				if rec.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("missing WWW-Authenticate header on 401")
				}
				// Same body for every reason: no hints for attackers.
				if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"unauthorized"}` {
					t.Fatalf("401 body = %q", got)
				}
			}
		})
	}
}
