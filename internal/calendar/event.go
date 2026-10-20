// Package calendar lists provider calendar events in a common shape.
package calendar

import (
	"context"
	"time"

	"github.com/sebasBetancourt/relay/internal/connect"
)

// Organizer is the optional event owner.
type Organizer struct {
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Event is the provider-neutral event returned to API clients.
type Event struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Start     string     `json:"start"` // RFC3339 or YYYY-MM-DD when AllDay
	End       string     `json:"end"`   // exclusive for all-day, like Google/Outlook
	Timezone  string     `json:"timezone,omitempty"`
	AllDay    bool       `json:"all_day"`
	Location  string     `json:"location,omitempty"`
	Organizer *Organizer `json:"organizer,omitempty"`
	Provider  string     `json:"provider"`
}

// Query is the list window and pagination.
type Query struct {
	TimeMin    time.Time
	TimeMax    time.Time
	MaxResults int
	PageToken  string
}

// Page is one page of events.
type Page struct {
	Events        []Event
	NextPageToken string
}

// Source talks to one provider's calendar API.
type Source interface {
	ListEvents(ctx context.Context, accessToken string, q Query) (Page, error)
}

// TokenProvider loads/refreshes OAuth access tokens (connect.Service).
type TokenProvider interface {
	AccessToken(ctx context.Context, tenantID, accountID string) (connect.AccessToken, error)
	RefreshAccessToken(ctx context.Context, tenantID, accountID string) (connect.AccessToken, error)
}
