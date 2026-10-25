// Package oauth runs the OAuth connect flow: Start builds the consent URL;
// Finish exchanges the callback code, encrypts tokens and persists the account.
// It knows nothing about HTTP; internal/httpapi maps its errors to status codes.
package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"github.com/sebasBetancourt/relay/internal/store"
)

// StateTTL is how long a user has to finish the consent screen.
const StateTTL = 10 * time.Minute

const stateBytes = 32 // 256 bits: unguessable CSRF token.

var (
	// ErrUnsupportedProvider is returned for providers Relay does not (yet) support.
	ErrUnsupportedProvider = errors.New("unsupported provider")
	// ErrConsentDenied: the user rejected the consent screen (error=access_denied).
	ErrConsentDenied = errors.New("consent denied")
	// ErrExchangeRejected: Google returned 4xx for the code/verifier (invalid_grant).
	ErrExchangeRejected = errors.New("token exchange rejected")
	// ErrProviderUnavailable: network error or Google 5xx / timeout.
	ErrProviderUnavailable = errors.New("provider unavailable")
	// ErrInsufficientScope: the user unchecked calendar.readonly.
	ErrInsufficientScope = errors.New("insufficient scope")
	// ErrInvalidIDToken: id_token missing or claims failed validation.
	ErrInvalidIDToken = errors.New("invalid id_token")
)

// StateStore persists and consumes one-time OAuth states.
type StateStore interface {
	Create(ctx context.Context, st store.OAuthState, ttl time.Duration) error
	Consume(ctx context.Context, state, provider string) (store.ConsumedState, error)
}

// AccountStore persists encrypted provider connections and token updates.
type AccountStore interface {
	SaveConnection(ctx context.Context, c store.Connection) (accountID string, err error)
	AccountForTenant(ctx context.Context, accountID, tenantID string) (store.StoredAccount, error)
	AccountByID(ctx context.Context, accountID string) (store.StoredAccount, error)
	// RefreshLocked runs fn under a per-account row lock shared by every
	// replica, and stores fn's update in the same transaction.
	RefreshLocked(ctx context.Context, accountID string, fn func(cur store.StoredAccount) (*store.TokenUpdate, error)) error
	MarkRevoked(ctx context.Context, accountID string) error
	// Disconnect sets status=disconnected, deletes the tokens and returns
	// the account as it was.
	Disconnect(ctx context.Context, accountID, tenantID string) (store.StoredAccount, error)
}

// Cipher is the vault surface (*vault.Vault implements it).
type Cipher interface {
	Encrypt(plaintext, aad []byte) (ciphertext, nonce []byte, err error)
	Decrypt(ciphertext, nonce, aad []byte) ([]byte, error)
}

// Deps is everything Service needs, built once in main.
type Deps struct {
	States     StateStore
	Accounts   AccountStore
	Vault      Cipher
	Providers  map[Provider]ProviderSpec
	HTTPClient *http.Client // timeout required; oauth2 defaults to http.DefaultClient (no timeout)
	Log        *slog.Logger
}

type Service struct {
	states    StateStore
	accounts  AccountStore
	vault     Cipher
	providers map[Provider]ProviderSpec
	http      *http.Client
	log       *slog.Logger
	flight    singleflight.Group
}

// NewService wires the OAuth service. HTTPClient must have a Timeout.
func NewService(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		states:    d.States,
		accounts:  d.Accounts,
		vault:     d.Vault,
		providers: maps.Clone(d.Providers),
		http:      d.HTTPClient,
		log:       log,
	}
}

// Result is returned after a successful Finish.
type Result struct {
	AccountID string
	Provider  Provider
	Email     string // may be empty
}

// Start begins a connect for tenantID and returns the consent URL.
// The state is saved before the URL is built: if saving fails, the user is
// never sent to a consent screen whose callback Relay would reject.
func (s *Service) Start(ctx context.Context, tenantID string, provider Provider) (string, error) {
	spec, ok := s.providers[provider]
	if !ok {
		return "", ErrUnsupportedProvider
	}

	state, err := newState()
	if err != nil {
		return "", err
	}
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

	// Provider options first, then PKCE: S256 is added last so no provider
	// spec can override the challenge.
	opts := append(append([]oauth2.AuthCodeOption{}, spec.AuthOptions...), oauth2.S256ChallengeOption(verifier))
	return spec.OAuth.AuthCodeURL(state, opts...), nil
}

// Finish completes the callback (design.md A.3–5): consume state, exchange
// code, encrypt tokens, save the connection. State is consumed first even on
// consent denial so it cannot be reused.
func (s *Service) Finish(ctx context.Context, provider Provider, state, code, providerErr string) (Result, error) {
	spec, ok := s.providers[provider]
	if !ok {
		return Result{}, ErrUnsupportedProvider
	}

	consumed, err := s.states.Consume(ctx, state, string(provider))
	if err != nil {
		return Result{}, err // ErrNotFound / ErrStateExpired / DB
	}

	if providerErr != "" {
		return Result{}, ErrConsentDenied
	}
	if code == "" {
		return Result{}, fmt.Errorf("finish connect: missing code")
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.http)
	tok, err := spec.OAuth.Exchange(ctx, code, oauth2.VerifierOption(consumed.CodeVerifier))
	if err != nil {
		return Result{}, mapExchangeError(err)
	}

	granted := grantedScopes(tok)
	if !spec.scopeGranted(granted) {
		return Result{}, ErrInsufficientScope
	}

	ident, err := spec.Identify(ctx, s.http, tok, spec.OAuth.ClientID)
	if err != nil {
		return Result{}, err
	}
	sub := ident.AccountID

	accessEnc, accessNonce, err := s.vault.Encrypt(
		[]byte(tok.AccessToken),
		tokenAAD(consumed.TenantID, string(provider), sub, "access_token"),
	)
	if err != nil {
		return Result{}, fmt.Errorf("finish connect: encrypt access: %w", err)
	}

	var refreshEnc, refreshNonce []byte
	if tok.RefreshToken != "" {
		refreshEnc, refreshNonce, err = s.vault.Encrypt(
			[]byte(tok.RefreshToken),
			tokenAAD(consumed.TenantID, string(provider), sub, "refresh_token"),
		)
		if err != nil {
			return Result{}, fmt.Errorf("finish connect: encrypt refresh: %w", err)
		}
	}

	accountID, err := s.accounts.SaveConnection(ctx, store.Connection{
		TenantID:          consumed.TenantID,
		Provider:          string(provider),
		ProviderAccountID: sub,
		Email:             ident.Email,
		AccessEnc:         accessEnc,
		AccessNonce:       accessNonce,
		RefreshEnc:        refreshEnc,
		RefreshNonce:      refreshNonce,
		ExpiresAt:         tok.Expiry,
		Scopes:            granted,
	})
	if err != nil {
		return Result{}, err
	}

	return Result{AccountID: accountID, Provider: provider, Email: ident.Email}, nil
}

func mapExchangeError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.Response != nil && re.Response.StatusCode >= 500 {
			return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
		}
		// 4xx: invalid_grant, bad verifier, used code, …
		return fmt.Errorf("%w: %v", ErrExchangeRejected, err)
	}
	// Network / timeout / cancelled.
	return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
}

func grantedScopes(tok *oauth2.Token) []string {
	raw, _ := tok.Extra("scope").(string)
	if raw == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Fields(raw) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func newState() (string, error) {
	b := make([]byte, stateBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("start connect: state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
