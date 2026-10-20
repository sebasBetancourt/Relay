// Package connect runs the OAuth connect flow: Start builds the consent URL;
// Finish exchanges the callback code, encrypts tokens and persists the account.
// It knows nothing about HTTP; internal/http maps its errors to status codes.
package connect

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/vault"
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

// AccountStore persists an encrypted provider connection.
type AccountStore interface {
	SaveConnection(ctx context.Context, c store.Connection) (accountID string, err error)
}

// Encrypter is the vault surface Finish needs (*vault.Vault implements it).
type Encrypter interface {
	Encrypt(plaintext, aad []byte) (ciphertext, nonce []byte, err error)
}

// Deps is everything Service needs, built once in main.
type Deps struct {
	States     StateStore
	Accounts   AccountStore
	Vault      Encrypter
	Providers  map[Provider]ProviderSpec
	HTTPClient *http.Client // timeout required; oauth2 defaults to http.DefaultClient (no timeout)
}

type Service struct {
	states    StateStore
	accounts  AccountStore
	vault     Encrypter
	providers map[Provider]ProviderSpec
	http      *http.Client
}

// NewService wires the connect service. HTTPClient must have a Timeout.
func NewService(d Deps) *Service {
	return &Service{
		states:    d.States,
		accounts:  d.Accounts,
		vault:     d.Vault,
		providers: maps.Clone(d.Providers),
		http:      d.HTTPClient,
	}
}

// Result is returned after a successful Finish.
type Result struct {
	AccountID string
	Provider  Provider
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

	return spec.OAuth.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(verifier),
	), nil
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
	if !scopeGranted(granted, spec.RequiredScope) {
		return Result{}, ErrInsufficientScope
	}

	sub, err := spec.Identify(tok, spec.OAuth.ClientID)
	if err != nil {
		return Result{}, err
	}

	accessEnc, accessNonce, err := s.vault.Encrypt(
		[]byte(tok.AccessToken),
		vault.AAD("oauth_tokens", consumed.TenantID, string(provider), sub, "access_token"),
	)
	if err != nil {
		return Result{}, fmt.Errorf("finish connect: encrypt access: %w", err)
	}

	var refreshEnc, refreshNonce []byte
	if tok.RefreshToken != "" {
		refreshEnc, refreshNonce, err = s.vault.Encrypt(
			[]byte(tok.RefreshToken),
			vault.AAD("oauth_tokens", consumed.TenantID, string(provider), sub, "refresh_token"),
		)
		if err != nil {
			return Result{}, fmt.Errorf("finish connect: encrypt refresh: %w", err)
		}
	}

	accountID, err := s.accounts.SaveConnection(ctx, store.Connection{
		TenantID:          consumed.TenantID,
		Provider:          string(provider),
		ProviderAccountID: sub,
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

	return Result{AccountID: accountID, Provider: provider}, nil
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

func scopeGranted(granted []string, required string) bool {
	for _, s := range granted {
		if s == required {
			return true
		}
	}
	return false
}

func newState() (string, error) {
	b := make([]byte, stateBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("start connect: state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
