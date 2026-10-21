package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/store"
)

type fakeAccountLister struct {
	rows      []store.AccountSummary
	err       error
	lastLimit int
	lastAfter *store.AccountCursor
}

func (f *fakeAccountLister) ListForTenant(_ context.Context, _ string, limit int, after *store.AccountCursor) ([]store.AccountSummary, error) {
	f.lastLimit, f.lastAfter = limit, after
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type fakeDisconnector struct {
	err                 error
	tenantID, accountID string
}

func (f *fakeDisconnector) Disconnect(_ context.Context, tenantID, accountID string) error {
	f.tenantID, f.accountID = tenantID, accountID
	return f.err
}

func accountsRouter(t *testing.T, key apikey.Key, l AccountLister, d Disconnector) http.Handler {
	t.Helper()
	return NewRouter(Deps{
		Log:        discardLogger(),
		Tenants:    &fakeLookup{tenants: map[string]store.Tenant{key.Hash: {ID: "tenant-1", Name: "T", KeyPrefix: key.KeyPrefix}}},
		Env:        apikey.EnvTest,
		Accounts:   l,
		Disconnect: d,
	})
}

func doAuthed(router http.Handler, method, path string, key apikey.Key) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func summaries(n int) []store.AccountSummary {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	out := make([]store.AccountSummary, n)
	for i := range out {
		out[i] = store.AccountSummary{
			ID: "acct-" + string(rune('a'+i)), Provider: "microsoft", Email: "u@ex.com",
			Status: "active", ConnectedAt: base.Add(time.Duration(i) * time.Minute),
		}
	}
	return out
}

func TestListAccounts(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	// The handler asks for limit+1 rows to know if a next page exists.
	lister := &fakeAccountLister{rows: summaries(3)}
	router := accountsRouter(t, key, lister, nil)

	rec := doAuthed(router, http.MethodGet, "/v1/accounts?limit=2", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if lister.lastLimit != 3 || lister.lastAfter != nil {
		t.Fatalf("store asked for limit=%d after=%v, want 3 and nil", lister.lastLimit, lister.lastAfter)
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("missing Cache-Control: private, no-store")
	}

	var body struct {
		Accounts []map[string]any `json:"accounts"`
		Next     string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 2 || body.Next == "" {
		t.Fatalf("body = %s, want 2 accounts and a next cursor", rec.Body.String())
	}
	a := body.Accounts[0]
	for _, k := range []string{"id", "provider", "email", "status", "connected_at"} {
		if _, ok := a[k]; !ok {
			t.Errorf("account JSON lacks %q", k)
		}
	}
	if len(a) != 5 {
		t.Fatalf("account JSON has unexpected fields (tokens must never appear): %v", a)
	}

	// The cursor points after the last returned row.
	rec = doAuthed(router, http.MethodGet, "/v1/accounts?limit=2&cursor="+body.Next, key)
	if rec.Code != http.StatusOK || lister.lastAfter == nil ||
		lister.lastAfter.ID != "acct-b" || !lister.lastAfter.CreatedAt.Equal(summaries(3)[1].ConnectedAt) {
		t.Fatalf("second page: status %d after %+v", rec.Code, lister.lastAfter)
	}
}

func TestListAccountsLastPageAndEmpty(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	router := accountsRouter(t, key, &fakeAccountLister{}, nil)

	rec := doAuthed(router, http.MethodGet, "/v1/accounts", key)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"accounts":[]}` {
		t.Fatalf("body = %s, want an empty array and no cursor", got)
	}
}

func TestListAccountsInvalidQuery(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	for _, q := range []string{"limit=0", "limit=101", "limit=x", "cursor=***", "cursor=bm90LWpzb24", "cursor=" + strings.Repeat("A", 600)} {
		lister := &fakeAccountLister{}
		rec := doAuthed(accountsRouter(t, key, lister, nil), http.MethodGet, "/v1/accounts?"+q, key)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_query") {
			t.Errorf("%s: status %d body %s", q, rec.Code, rec.Body.String())
		}
	}
	// A well-formed cursor whose id is not a uuid: the store says not found.
	cursor := encodeAccountsCursor(cursorJSON{CreatedAt: time.Now(), ID: "nope"})
	rec := doAuthed(accountsRouter(t, key, &fakeAccountLister{err: store.ErrNotFound}, nil), http.MethodGet, "/v1/accounts?cursor="+cursor, key)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDisconnectAccount(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"disconnected", nil, http.StatusNoContent},
		{"missing or another tenant's", store.ErrNotFound, http.StatusNotFound},
		{"store failure", errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDisconnector{err: tt.err}
			rec := doAuthed(accountsRouter(t, key, nil, d), http.MethodDelete, "/v1/accounts/acct-9", key)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if d.tenantID != "tenant-1" || d.accountID != "acct-9" {
				t.Fatalf("disconnect(%q, %q), want tenant-1/acct-9", d.tenantID, d.accountID)
			}
			if tt.wantStatus == http.StatusNoContent && rec.Body.Len() != 0 {
				t.Fatal("204 must have no body")
			}
		})
	}
}

func TestAccountsRequireAPIKey(t *testing.T) {
	key := apikey.Generate(apikey.EnvTest)
	d := &fakeDisconnector{}
	router := accountsRouter(t, key, &fakeAccountLister{}, d)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		path := "/v1/accounts"
		if method == http.MethodDelete {
			path += "/acct-9"
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without key: status %d", method, rec.Code)
		}
	}
	if d.accountID != "" {
		t.Fatal("disconnect must not run without an API key")
	}
}
