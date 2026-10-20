package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	googleCalendarAPI = "https://www.googleapis.com/calendar/v3"
	maxGoogleBody     = 5 << 20 // 5 MiB
)

var (
	// ErrTokenRejected: Google returned 401 for this access token.
	ErrTokenRejected = errors.New("token rejected")
	// ErrCalendarNotFound: primary calendar missing (404).
	ErrCalendarNotFound = errors.New("calendar not found")
	// ErrProviderUnavailable: network error or Google 5xx.
	ErrProviderUnavailable = errors.New("provider unavailable")
	// ErrProviderBadResponse: unexpected 4xx or unparseable body.
	ErrProviderBadResponse = errors.New("provider bad response")
)

// RateLimitedError is a 429 (or 403 rateLimitExceeded) from Google.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string { return "provider rate limited" }

// GoogleSource calls Google Calendar API v3.
type GoogleSource struct {
	BaseURL string
	HTTP    *http.Client
}

// NewGoogleSource builds a production Google Calendar client.
func NewGoogleSource(httpClient *http.Client) *GoogleSource {
	return &GoogleSource{BaseURL: googleCalendarAPI, HTTP: httpClient}
}

type googleEventsResponse struct {
	TimeZone      string        `json:"timeZone"`
	NextPageToken string        `json:"nextPageToken"`
	Items         []googleEvent `json:"items"`
}

type googleEvent struct {
	ID        string          `json:"id"`
	Summary   string          `json:"summary"`
	Location  string          `json:"location"`
	Start     googleDateTime  `json:"start"`
	End       googleDateTime  `json:"end"`
	Organizer *googlePerson   `json:"organizer"`
}

type googleDateTime struct {
	Date     string `json:"date"`     // YYYY-MM-DD all-day
	DateTime string `json:"dateTime"` // RFC3339 timed
	TimeZone string `json:"timeZone"`
}

type googlePerson struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

type googleErrorBody struct {
	Error struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
		Message string `json:"message"`
	} `json:"error"`
}

// ListEvents fetches primary calendar events in the query window.
func (g *GoogleSource) ListEvents(ctx context.Context, accessToken string, q Query) (Page, error) {
	base := strings.TrimRight(g.BaseURL, "/")
	u, err := url.Parse(base + "/calendars/primary/events")
	if err != nil {
		return Page{}, fmt.Errorf("google list: %w", err)
	}
	vals := url.Values{}
	vals.Set("singleEvents", "true")
	vals.Set("orderBy", "startTime")
	vals.Set("timeMin", q.TimeMin.UTC().Format(time.RFC3339))
	vals.Set("timeMax", q.TimeMax.UTC().Format(time.RFC3339))
	if q.MaxResults > 0 {
		vals.Set("maxResults", strconv.Itoa(q.MaxResults))
	}
	if q.PageToken != "" {
		vals.Set("pageToken", q.PageToken)
	}
	u.RawQuery = vals.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Page{}, fmt.Errorf("google list: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxGoogleBody+1))
	if err != nil {
		return Page{}, fmt.Errorf("%w: read body: %v", ErrProviderUnavailable, err)
	}
	if len(body) > maxGoogleBody {
		return Page{}, ErrProviderBadResponse
	}

	switch {
	case res.StatusCode == http.StatusOK:
		return parseGoogleEvents(body)
	case res.StatusCode == http.StatusUnauthorized:
		return Page{}, ErrTokenRejected
	case res.StatusCode == http.StatusNotFound:
		return Page{}, ErrCalendarNotFound
	case res.StatusCode == http.StatusTooManyRequests:
		return Page{}, &RateLimitedError{RetryAfter: parseRetryAfter(res.Header.Get("Retry-After"))}
	case res.StatusCode == http.StatusForbidden && isGoogleRateLimit(body):
		return Page{}, &RateLimitedError{RetryAfter: parseRetryAfter(res.Header.Get("Retry-After"))}
	case res.StatusCode >= 500:
		return Page{}, fmt.Errorf("%w: status %d", ErrProviderUnavailable, res.StatusCode)
	default:
		return Page{}, fmt.Errorf("%w: status %d", ErrProviderBadResponse, res.StatusCode)
	}
}

func parseGoogleEvents(body []byte) (Page, error) {
	var raw googleEventsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return Page{}, fmt.Errorf("%w: %v", ErrProviderBadResponse, err)
	}
	events := make([]Event, 0, len(raw.Items))
	for _, item := range raw.Items {
		events = append(events, normalizeGoogleEvent(item, raw.TimeZone))
	}
	return Page{Events: events, NextPageToken: raw.NextPageToken}, nil
}

func normalizeGoogleEvent(item googleEvent, calTZ string) Event {
	ev := Event{
		ID:       item.ID,
		Title:    item.Summary, // empty stays empty — do not invent a title
		Location: item.Location,
		Provider: "google",
	}
	if item.Organizer != nil && (item.Organizer.Email != "" || item.Organizer.DisplayName != "") {
		ev.Organizer = &Organizer{Email: item.Organizer.Email, Name: item.Organizer.DisplayName}
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

func isGoogleRateLimit(body []byte) bool {
	var eb googleErrorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		return false
	}
	for _, e := range eb.Error.Errors {
		if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
			return true
		}
	}
	return false
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}
