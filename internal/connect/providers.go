package connect

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

// ProviderSpec is everything Service needs for one OAuth provider. Adding
// Microsoft later means another map entry, not a rewrite of Finish.
type ProviderSpec struct {
	OAuth         *oauth2.Config
	RequiredScope string
	// Identify returns provider_account_id from the token response.
	Identify func(tok *oauth2.Token, clientID string) (string, error)
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
		Scopes:       []string{GoogleOpenID, GoogleCalendarReadonly},
	}
}

// GoogleSpec wraps GoogleConfig with identify + required calendar scope.
func GoogleSpec(clientID, clientSecret, redirectURL string) ProviderSpec {
	return ProviderSpec{
		OAuth:         GoogleConfig(clientID, clientSecret, redirectURL),
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
func googleIdentify(tok *oauth2.Token, clientID string) (string, error) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return "", fmt.Errorf("%w: missing id_token", ErrInvalidIDToken)
	}
	claims, err := decodeJWTPayload(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidIDToken, err)
	}

	iss, _ := claims["iss"].(string)
	if iss != "https://accounts.google.com" && iss != "accounts.google.com" {
		return "", fmt.Errorf("%w: bad iss", ErrInvalidIDToken)
	}
	aud, ok := claimAudience(claims["aud"])
	if !ok || aud != clientID {
		return "", fmt.Errorf("%w: bad aud", ErrInvalidIDToken)
	}
	exp, ok := claimUnix(claims["exp"])
	if !ok || !time.Now().Before(time.Unix(exp, 0)) {
		return "", fmt.Errorf("%w: expired", ErrInvalidIDToken)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", fmt.Errorf("%w: empty sub", ErrInvalidIDToken)
	}
	return sub, nil
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
