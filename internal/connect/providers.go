package connect

import (
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
)

// Provider names match the Postgres enum `provider`.
type Provider string

const (
	ProviderGoogle    Provider = "google"
	ProviderMicrosoft Provider = "microsoft"
)

// GoogleCalendarReadonly is the only Google scope v0.1 asks for (read events).
const GoogleCalendarReadonly = "https://www.googleapis.com/auth/calendar.readonly"

// GoogleConfig builds the OAuth client for Google. It uses
// oauth2/endpoints instead of oauth2/google, which would pull GCP
// dependencies Relay does not need.
func GoogleConfig(clientID, clientSecret, redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     endpoints.Google,
		Scopes:       []string{GoogleCalendarReadonly},
	}
}
