package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
)

// Microsoft Graph delegated scopes v0.1 asks for. Short names resolve to
// Microsoft Graph (https://graph.microsoft.com/...).
const (
	MicrosoftCalendarsRead = "Calendars.Read"
	MicrosoftUserRead      = "User.Read"
	// MicrosoftCalendarsReadWrite adds event writes, asked for only with
	// FeatureCalendarWrite.
	MicrosoftCalendarsReadWrite = "Calendars.ReadWrite"
	// MicrosoftOfflineAccess makes Entra ID return a refresh token.
	MicrosoftOfflineAccess = "offline_access"

	// MicrosoftGraphURL is the production Graph base URL.
	MicrosoftGraphURL = "https://graph.microsoft.com"

	microsoftGraphResource = "https://graph.microsoft.com/"
	maxMeBody              = 1 << 20 // 1 MiB: /me is a few hundred bytes
)

// ErrIdentityLookup: the provider could not tell who the account is
// (e.g. Graph /me answered 4xx or without an id).
var ErrIdentityLookup = errors.New("identity lookup failed")

// MicrosoftConfig builds the OAuth client for Entra ID (Microsoft identity
// platform v2). tenant is "common", "organizations", "consumers", a
// directory GUID or a domain; config validates it before it gets here.
func MicrosoftConfig(clientID, clientSecret, redirectURL, tenant string) *oauth2.Config {
	ep := endpoints.AzureAD(tenant)
	// One deterministic request: with AuthStyleAutoDetect a failed exchange
	// is retried with the other style, doubling calls and error noise.
	ep.AuthStyle = oauth2.AuthStyleInParams
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     ep,
		Scopes: []string{
			GoogleOpenID, // "openid": standard OIDC scope, same value for every provider
			GoogleEmail,  // "email"
			MicrosoftOfflineAccess,
			MicrosoftUserRead,
			MicrosoftCalendarsRead,
		},
	}
}

// MicrosoftSpec wires Entra ID + Microsoft Graph. graphBaseURL is
// MicrosoftGraphURL in production and a fake server in tests.
func MicrosoftSpec(clientID, clientSecret, redirectURL, tenant, graphBaseURL string) ProviderSpec {
	return ProviderSpec{
		OAuth: MicrosoftConfig(clientID, clientSecret, redirectURL, tenant),
		// No access_type (Google-only) and no forced consent: offline_access
		// already yields a refresh token. select_account lets users with a
		// work and a personal account pick the right one.
		AuthOptions:   []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("prompt", "select_account")},
		RequiredScope: MicrosoftCalendarsRead,
		HasScope:      microsoftHasScope(MicrosoftCalendarsRead),
		Identify:      microsoftIdentify(graphBaseURL),
	}
}

// microsoftHasScope accepts the short Graph name or its full resource URI
// ("Calendars.Read" or "https://graph.microsoft.com/Calendars.Read"),
// case-insensitively, as Entra ID may return either.
func microsoftHasScope(required string) func([]string) bool {
	return func(granted []string) bool {
		for _, s := range granted {
			if strings.EqualFold(strings.TrimPrefix(strings.ToLower(s), microsoftGraphResource), required) {
				return true
			}
		}
		return false
	}
}

type graphMe struct {
	ID                string `json:"id"`
	Mail              string `json:"mail"`
	UserPrincipalName string `json:"userPrincipalName"`
}

// microsoftIdentify asks Graph who the token belongs to. `id` is permanent
// (it is the provider_account_id and part of the token AAD); `mail` is
// often empty for personal accounts, so userPrincipalName is the fallback.
func microsoftIdentify(graphBaseURL string) func(context.Context, *http.Client, *oauth2.Token, string) (Identity, error) {
	meURL := strings.TrimRight(graphBaseURL, "/") + "/v1.0/me?$select=id,mail,userPrincipalName"

	return func(ctx context.Context, c *http.Client, tok *oauth2.Token, _ string) (Identity, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, meURL, nil)
		if err != nil {
			return Identity{}, fmt.Errorf("%w: %v", ErrIdentityLookup, err)
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		req.Header.Set("Accept", "application/json")

		resp, err := c.Do(req)
		if err != nil {
			return Identity{}, fmt.Errorf("%w: graph /me: %v", ErrProviderUnavailable, err)
		}
		defer func() { _ = resp.Body.Close() }()

		switch {
		case resp.StatusCode >= 500:
			return Identity{}, fmt.Errorf("%w: graph /me status %d", ErrProviderUnavailable, resp.StatusCode)
		case resp.StatusCode != http.StatusOK:
			return Identity{}, fmt.Errorf("%w: graph /me status %d", ErrIdentityLookup, resp.StatusCode)
		}

		var me graphMe
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxMeBody)).Decode(&me); err != nil {
			return Identity{}, fmt.Errorf("%w: graph /me body: %v", ErrIdentityLookup, err)
		}
		if me.ID == "" {
			return Identity{}, fmt.Errorf("%w: graph /me without id", ErrIdentityLookup)
		}

		email := me.Mail
		if email == "" {
			email = me.UserPrincipalName
		}
		return Identity{AccountID: me.ID, Email: email}, nil
	}
}
