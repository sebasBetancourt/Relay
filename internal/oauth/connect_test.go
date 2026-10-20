package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/store"
)

type fakeStates struct {
	saved   []store.OAuthState
	ttls    []time.Duration
	err     error
	consume map[string]store.ConsumedState
	consErr error
}

func (f *fakeStates) Create(_ context.Context, st store.OAuthState, ttl time.Duration) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, st)
	f.ttls = append(f.ttls, ttl)
	return nil
}

func (f *fakeStates) Consume(_ context.Context, state, provider string) (store.ConsumedState, error) {
	if f.consErr != nil {
		return store.ConsumedState{}, f.consErr
	}
	key := provider + ":" + state
	if f.consume != nil {
		if c, ok := f.consume[key]; ok {
			delete(f.consume, key)
			return c, nil
		}
	}
	// Default: match last Create for this state+provider.
	for i := len(f.saved) - 1; i >= 0; i-- {
		st := f.saved[i]
		if st.State == state && st.Provider == provider {
			f.saved = append(f.saved[:i], f.saved[i+1:]...)
			return store.ConsumedState{TenantID: st.TenantID, CodeVerifier: st.CodeVerifier}, nil
		}
	}
	return store.ConsumedState{}, store.ErrNotFound
}

const redirect = "http://localhost:8080/v1/oauth/google/callback"

func newTestService(states StateStore) *Service {
	return NewService(Deps{
		States: states,
		Providers: map[Provider]ProviderSpec{
			ProviderGoogle: GoogleSpec("client-123", "secret-xyz", redirect),
		},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
}

func TestStartGoogle(t *testing.T) {
	states := &fakeStates{}
	svc := newTestService(states)

	raw, err := svc.Start(context.Background(), "tenant-1", ProviderGoogle)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(states.saved) != 1 {
		t.Fatalf("saved %d states, want 1", len(states.saved))
	}
	saved := states.saved[0]
	if saved.TenantID != "tenant-1" || saved.Provider != "google" {
		t.Fatalf("saved %+v", saved)
	}
	if states.ttls[0] != 10*time.Minute {
		t.Fatalf("ttl = %v, want 10m", states.ttls[0])
	}
	if len(saved.State) != 43 {
		t.Fatalf("state length = %d, want 43", len(saved.State))
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "accounts.google.com" {
		t.Fatalf("auth URL host = %s://%s", u.Scheme, u.Host)
	}

	sum := sha256.Sum256([]byte(saved.CodeVerifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(sum[:])

	q := u.Query()
	want := map[string]string{
		"state":                 saved.State,
		"code_challenge":        wantChallenge,
		"code_challenge_method": "S256",
		"access_type":           "offline",
		"prompt":                "consent",
		"response_type":         "code",
		"client_id":             "client-123",
		"redirect_uri":          redirect,
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	scope := q.Get("scope")
	if !containsScope(scope, GoogleOpenID) || !containsScope(scope, GoogleCalendarReadonly) {
		t.Errorf("scope = %q, want openid + calendar.readonly", scope)
	}
	for _, k := range []string{"client_secret", "code_verifier"} {
		if q.Has(k) {
			t.Errorf("auth URL leaks %s", k)
		}
	}
}

func containsScope(spaceSeparated, want string) bool {
	for _, s := range splitScopes(spaceSeparated) {
		if s == want {
			return true
		}
	}
	return false
}

func splitScopes(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func TestStartGeneratesFreshValues(t *testing.T) {
	states := &fakeStates{}
	svc := newTestService(states)

	for range 2 {
		if _, err := svc.Start(context.Background(), "tenant-1", ProviderGoogle); err != nil {
			t.Fatal(err)
		}
	}
	a, b := states.saved[0], states.saved[1]
	if a.State == b.State || a.CodeVerifier == b.CodeVerifier {
		t.Fatal("state and verifier must be fresh on every call")
	}
}

func TestStartUnsupportedProvider(t *testing.T) {
	states := &fakeStates{}
	svc := newTestService(states)

	for _, p := range []Provider{ProviderMicrosoft, "yahoo", ""} {
		_, err := svc.Start(context.Background(), "tenant-1", p)
		if !errors.Is(err, ErrUnsupportedProvider) {
			t.Fatalf("%q: err = %v, want ErrUnsupportedProvider", p, err)
		}
	}
	if len(states.saved) != 0 {
		t.Fatal("unsupported provider must not touch the store")
	}
}

func TestStartStoreFailure(t *testing.T) {
	boom := errors.New("db down")
	svc := newTestService(&fakeStates{err: boom})

	raw, err := svc.Start(context.Background(), "tenant-1", ProviderGoogle)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped db error", err)
	}
	if raw != "" {
		t.Fatal("no consent URL may be returned when the state was not saved")
	}
}
