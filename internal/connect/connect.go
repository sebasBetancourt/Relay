// Package connect runs the OAuth connect flow: it creates the one-time
// state and PKCE verifier, persists them, and builds the provider's consent
// URL. It knows nothing about HTTP; internal/http maps its errors to
// status codes.
package connect

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/store"
)

// StateTTL is how long a user has to finish the consent screen.
const StateTTL = 10 * time.Minute

const stateBytes = 32 // 256 bits: unguessable CSRF token.

// ErrUnsupportedProvider is returned for providers Relay does not (yet) support.
var ErrUnsupportedProvider = errors.New("unsupported provider")

// StateStore is the only thing Service needs from the store, so tests can
// use a fake instead of Postgres.
type StateStore interface {
	Create(ctx context.Context, st store.OAuthState, ttl time.Duration) error
}

type Service struct {
	states    StateStore
	providers map[Provider]*oauth2.Config
}

// NewService takes the OAuth client of every enabled provider. A provider
// missing from the map is unsupported.
func NewService(states StateStore, providers map[Provider]*oauth2.Config) *Service {
	return &Service{states: states, providers: maps.Clone(providers)}
}

// Start begins a connect for tenantID and returns the consent URL.
// The state is saved before the URL is built: if saving fails, the user is
// never sent to a consent screen whose callback Relay would reject.
func (s *Service) Start(ctx context.Context, tenantID string, provider Provider) (string, error) {
	cfg, ok := s.providers[provider]
	if !ok {
		return "", ErrUnsupportedProvider
	}

	state, err := newState()
	if err != nil {
		return "", err
	}
	// RFC 7636 verifier (32 random bytes, base64url). The library is audited;
	// the tests recompute the challenge by hand to show it is SHA-256 + base64url.
	verifier := oauth2.GenerateVerifier()

	err = s.states.Create(ctx, store.OAuthState{
		State:        state,
		TenantID:     tenantID,
		Provider:     string(provider),
		CodeVerifier: verifier,
	}, StateTTL)
	if err != nil {
		return "", fmt.Errorf("start connect: %w", err)
	}

	return cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,             // access_type=offline → Google returns a refresh_token
		oauth2.ApprovalForce,                 // prompt=consent → refresh_token even on reconnect
		oauth2.S256ChallengeOption(verifier), // code_challenge=base64url(sha256(verifier)), method S256
	), nil
}

func newState() (string, error) {
	b := make([]byte, stateBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("start connect: state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
