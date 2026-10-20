package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
)

// Provider names match the Postgres enum `provider`.
type Provider string

const (
	ProviderGoogle    Provider = "google"
	ProviderMicrosoft Provider = "microsoft"
)

// GoogleCalendarReadonly is the calendar scope v0.1 asks for (read events).
const GoogleCalendarReadonly = "https://www.googleapis.com/auth/calendar.readonly"

// GoogleOpenID is required so the token response includes an id_token with
// `sub`, the permanent Google account id used as provider_account_id.
const GoogleOpenID = "openid"

// GoogleEmail adds the account's email (and email_verified) to the id_token.
const GoogleEmail = "email"

// Identity is who the connected provider account is.
type Identity struct {
	// AccountID is the provider's permanent id (Google `sub`, Microsoft
	// Graph `id`), stored as provider_account_id and bound into the AAD.
	AccountID string
	// Email is informational and may change or be empty; never use it as a key.
	Email string
}

// ProviderSpec is everything Service needs for one OAuth provider. Start,
// Finish and refresh never branch on the provider: adding one means a new
// spec, not a rewrite of the flow.
type ProviderSpec struct {
	OAuth *oauth2.Config
	// AuthOptions are provider-specific consent URL parameters
	// (state and PKCE are always added by Start).
	AuthOptions []oauth2.AuthCodeOption
	// RequiredScope must be granted, or Finish returns ErrInsufficientScope.
	RequiredScope string
	// HasScope reports whether RequiredScope is among the granted scopes.
	// Nil means an exact match.
	HasScope func(granted []string) bool
	// Identify returns who the account is, using the token response and, if
	// needed, an API call with the injected HTTP client.
	Identify func(ctx context.Context, c *http.Client, tok *oauth2.Token, clientID string) (Identity, error)
}

func (p ProviderSpec) scopeGranted(granted []string) bool {
	if p.HasScope != nil {
		return p.HasScope(granted)
	}
	for _, s := range granted {
		if s == p.RequiredScope {
			return true
		}
	}
	return false
}

// GoogleConfig builds the OAuth client for Google. It uses
// oauth2/endpoints instead of oauth2/google, which would pull GCP
// dependencies Relay does not need.
func GoogleConfig(clientID, clientSecret, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     endpoints.Google,
		Scopes:       []string{GoogleOpenID, GoogleEmail, GoogleCalendarReadonly},
	}
}

// GoogleSpec wraps GoogleConfig with identify + required calendar scope.
func GoogleSpec(clientID, clientSecret, redirectURL string) ProviderSpec {
	return ProviderSpec{
		OAuth: GoogleConfig(clientID, clientSecret, redirectURL),
		AuthOptions: []oauth2.AuthCodeOption{
			oauth2.AccessTypeOffline, // access_type=offline → Google returns a refresh_token
			oauth2.ApprovalForce,     // prompt=consent → refresh_token even on reconnect
		},
		RequiredScope: GoogleCalendarReadonly,
		Identify:      googleIdentify,
	}
}

// googleIdentify reads the id_token from the token endpoint response.
//
// Signature verification is intentionally skipped: OIDC Core §3.1.3.7 allows
// that when the id_token is obtained directly from the token endpoint over
// TLS (as oauth2.Exchange does). If the id_token ever arrived via the
// browser, JWKS verification would be required.
//
// The email is kept only when Google marks it verified.
func googleIdentify(_ context.Context, _ *http.Client, tok *oauth2.Token, clientID string) (Identity, error) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, fmt.Errorf("%w: missing id_token", ErrInvalidIDToken)
	}
	claims, err := decodeJWTPayload(raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalidIDToken, err)
	}

	iss, _ := claims["iss"].(string)
	if iss != "https://accounts.google.com" && iss != "accounts.google.com" {
		return Identity{}, fmt.Errorf("%w: bad iss", ErrInvalidIDToken)
	}
	aud, ok := claimAudience(claims["aud"])
	if !ok || aud != clientID {
		return Identity{}, fmt.Errorf("%w: bad aud", ErrInvalidIDToken)
	}
	exp, ok := claimUnix(claims["exp"])
	if !ok || !time.Now().Before(time.Unix(exp, 0)) {
		return Identity{}, fmt.Errorf("%w: expired", ErrInvalidIDToken)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Identity{}, fmt.Errorf("%w: empty sub", ErrInvalidIDToken)
	}

	id := Identity{AccountID: sub}
	if verified, _ := claims["email_verified"].(bool); verified {
		id.Email, _ = claims["email"].(string)
	}
	return id, nil
}

func decodeJWTPayload(jwt string) (map[string]any, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func claimAudience(v any) (string, bool) {
	switch a := v.(type) {
	case string:
		return a, a != ""
	case []any:
		if len(a) == 0 {
			return "", false
		}
		s, ok := a[0].(string)
		return s, ok && s != ""
	default:
		return "", false
	}
}

func claimUnix(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}
