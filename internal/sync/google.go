package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sebasBetancourt/relay/internal/calendar"
)

const googleCalendarAPI = "https://www.googleapis.com/calendar/v3"

// Google reads events.list with a syncToken. Incremental calls send the
// token and not the window: Google rejects a syncToken combined with
// timeMin, timeMax or orderBy.
type Google struct {
	BaseURL string
	HTTP    *http.Client
}

// NewGoogle builds a production Calendar API client.
func NewGoogle(httpClient *http.Client) *Google {
	return &Google{BaseURL: googleCalendarAPI, HTTP: httpClient}
}

type googlePage struct {
	TimeZone      string        `json:"timeZone"`
	NextPageToken string        `json:"nextPageToken"`
	NextSyncToken string        `json:"nextSyncToken"`
	Items         []googleEvent `json:"items"`
}

type googleEvent struct {
	ID        string        `json:"id"`
	Etag      string        `json:"etag"`
	Status    string        `json:"status"`
	Summary   string        `json:"summary"`
	Location  string        `json:"location"`
	Start     googleWhen    `json:"start"`
	End       googleWhen    `json:"end"`
	Organizer *googlePerson `json:"organizer"`
}

type googleWhen struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type googlePerson struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

// Delta lists every page until Google returns nextSyncToken.
func (g *Google) Delta(ctx context.Context, accessToken, cursor string, window Window) ([]Change, string, error) {
	var all []Change
	pageToken := ""
	for page := 0; page < maxDeltaPages; page++ {
		body, status, err := g.get(ctx, accessToken, cursor, window, pageToken)
		if err != nil {
			return nil, "", err
		}
		if status == http.StatusGone {
			return nil, "", ErrCursorExpired
		}
		if err := googleStatus(status, body); err != nil {
			return nil, "", err
		}
		var raw googlePage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, "", fmt.Errorf("%w: %v", calendar.ErrProviderBadResponse, err)
		}
		for _, item := range raw.Items {
			if item.ID == "" {
				return nil, "", fmt.Errorf("%w: event without id", calendar.ErrProviderBadResponse)
			}
			all = append(all, Change{
				Event:   normalizeGoogle(item, raw.TimeZone),
				Version: item.Etag,
				Deleted: item.Status == "cancelled",
			})
		}
		if raw.NextSyncToken != "" {
			return all, raw.NextSyncToken, nil
		}
		if raw.NextPageToken == "" {
			return nil, "", fmt.Errorf("%w: events.list ended without a sync token", calendar.ErrProviderBadResponse)
		}
		pageToken = raw.NextPageToken
	}
	return nil, "", fmt.Errorf("%w: too many events.list pages", calendar.ErrProviderBadResponse)
}

func (g *Google) get(ctx context.Context, accessToken, cursor string, window Window, pageToken string) ([]byte, int, error) {
	base := strings.TrimRight(g.BaseURL, "/")
	u, err := url.Parse(base + "/calendars/primary/events")
	if err != nil {
		return nil, 0, fmt.Errorf("google delta: %w", err)
	}
	vals := url.Values{}
	vals.Set("singleEvents", "true")
	vals.Set("maxResults", "250")
	if cursor == "" {
		vals.Set("timeMin", window.Start.UTC().Format(time.RFC3339))
		vals.Set("timeMax", window.End.UTC().Format(time.RFC3339))
	} else {
		// The token already carries the window. Sending it again is a 400.
		vals.Set("syncToken", cursor)
	}
	if pageToken != "" {
		vals.Set("pageToken", pageToken)
	}
	u.RawQuery = vals.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("google delta: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", calendar.ErrProviderUnavailable, scrubURL(err))
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, 5<<20+1))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: read body", calendar.ErrProviderUnavailable)
	}
	if len(body) > 5<<20 {
		return nil, 0, calendar.ErrProviderBadResponse
	}
	return body, res.StatusCode, nil
}

func googleStatus(status int, body []byte) error {
	switch {
	case status == http.StatusOK:
		return nil
	case status == http.StatusUnauthorized:
		return calendar.ErrTokenRejected
	case status == http.StatusTooManyRequests:
		return &calendar.RateLimitedError{}
	case status >= 500:
		return fmt.Errorf("%w: status %d", calendar.ErrProviderUnavailable, status)
	default:
		return fmt.Errorf("%w: status %d", calendar.ErrProviderBadResponse, status)
	}
}

func normalizeGoogle(item googleEvent, calTZ string) calendar.Event {
	ev := calendar.Event{
		ID:       item.ID,
		Title:    item.Summary,
		Location: item.Location,
		Provider: "google",
	}
	if item.Organizer != nil && (item.Organizer.Email != "" || item.Organizer.DisplayName != "") {
		ev.Organizer = &calendar.Organizer{Email: item.Organizer.Email, Name: item.Organizer.DisplayName}
	}
	switch {
	case item.Start.Date != "":
		ev.AllDay = true
		ev.Start = item.Start.Date
		ev.End = item.End.Date
	default:
		ev.Start = item.Start.DateTime
		ev.End = item.End.DateTime
		tz := item.Start.TimeZone
		if tz == "" {
			tz = calTZ
		}
		ev.Timezone = tz
	}
	return ev
}

// scrubURL drops the request URL. A Google error can echo the syncToken.
func scrubURL(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + ": " + ue.Err.Error()
	}
	msg := err.Error()
	if i := strings.Index(msg, "http"); i >= 0 {
		return strings.TrimSpace(msg[:i])
	}
	return msg
}
