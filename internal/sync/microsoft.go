package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sebasBetancourt/relay/internal/calendar"
)

const (
	microsoftGraphURL   = "https://graph.microsoft.com"
	graphDateTimeLayout = "2006-01-02T15:04:05.9999999"
)

// Microsoft reads /me/calendarView/delta. nextLink and deltaLink are
// followed only when the host is the configured Graph host. A foreign
// host would send the user's token off Graph.
type Microsoft struct {
	BaseURL string
	HTTP    *http.Client
}

// NewMicrosoft builds a production Graph client.
func NewMicrosoft(httpClient *http.Client) *Microsoft {
	return &Microsoft{BaseURL: microsoftGraphURL, HTTP: httpClient}
}

type graphPage struct {
	Value     []graphEvent `json:"value"`
	NextLink  string       `json:"@odata.nextLink"`
	DeltaLink string       `json:"@odata.deltaLink"`
}

type graphEvent struct {
	ID                    string    `json:"id"`
	ChangeKey             string    `json:"changeKey"`
	Etag                  string    `json:"@odata.etag"`
	Subject               string    `json:"subject"`
	IsAllDay              bool      `json:"isAllDay"`
	IsCancelled           bool      `json:"isCancelled"`
	OriginalStartTimeZone string    `json:"originalStartTimeZone"`
	Start                 graphWhen `json:"start"`
	End                   graphWhen `json:"end"`
	Location              struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	Organizer *struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"organizer"`
	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
}

type graphWhen struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// Delta follows @odata.nextLink until @odata.deltaLink.
func (m *Microsoft) Delta(ctx context.Context, accessToken, cursor string, window Window) ([]Change, string, error) {
	nextURL, err := m.firstURL(cursor, window)
	if err != nil {
		return nil, "", err
	}
	var all []Change
	for page := 0; page < maxDeltaPages; page++ {
		if page > 0 || cursor != "" {
			if err := m.allow(nextURL); err != nil {
				return nil, "", err
			}
		}
		body, status, err := m.get(ctx, accessToken, nextURL)
		if err != nil {
			return nil, "", err
		}
		if status == http.StatusGone {
			return nil, "", ErrCursorExpired
		}
		if err := graphStatus(status, body); err != nil {
			return nil, "", err
		}
		var raw graphPage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, "", fmt.Errorf("%w: %v", calendar.ErrProviderBadResponse, err)
		}
		for _, item := range raw.Value {
			ch, err := normalizeGraph(item)
			if err != nil {
				return nil, "", err
			}
			all = append(all, ch)
		}
		if raw.DeltaLink != "" {
			if err := m.allow(raw.DeltaLink); err != nil {
				return nil, "", err
			}
			return all, raw.DeltaLink, nil
		}
		if raw.NextLink == "" {
			return nil, "", fmt.Errorf("%w: delta ended without a deltaLink", calendar.ErrProviderBadResponse)
		}
		nextURL = raw.NextLink
	}
	return nil, "", fmt.Errorf("%w: too many delta pages", calendar.ErrProviderBadResponse)
}

func (m *Microsoft) firstURL(cursor string, window Window) (string, error) {
	if cursor != "" {
		return cursor, nil
	}
	vals := url.Values{}
	vals.Set("startDateTime", window.Start.UTC().Format(time.RFC3339))
	vals.Set("endDateTime", window.End.UTC().Format(time.RFC3339))
	base := strings.TrimRight(m.BaseURL, "/")
	return base + "/v1.0/me/calendarView/delta?" + vals.Encode(), nil
}

// allow refuses any link whose host is not the configured Graph host.
func (m *Microsoft) allow(raw string) error {
	base, err := url.Parse(m.BaseURL)
	if err != nil || base.Host == "" {
		return fmt.Errorf("%w: bad graph base url", calendar.ErrProviderBadResponse)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("%w: bad delta link", calendar.ErrProviderBadResponse)
	}
	if !strings.EqualFold(u.Host, base.Host) || u.Scheme != base.Scheme {
		return fmt.Errorf("%w: foreign delta link", calendar.ErrProviderBadResponse)
	}
	if !strings.Contains(u.Path, "/calendarView/delta") {
		return fmt.Errorf("%w: unexpected delta path", calendar.ErrProviderBadResponse)
	}
	return nil
}

func (m *Microsoft) get(ctx context.Context, accessToken, endpoint string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("graph delta: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Prefer", `outlook.timezone="UTC"`)

	client := m.HTTP
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

func graphStatus(status int, _ []byte) error {
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

func normalizeGraph(item graphEvent) (Change, error) {
	if item.ID == "" {
		return Change{}, fmt.Errorf("%w: event without id", calendar.ErrProviderBadResponse)
	}
	version := item.ChangeKey
	if version == "" {
		version = item.Etag
	}
	if item.Removed != nil || item.IsCancelled {
		if version == "" {
			version = "removed"
		}
		return Change{
			Event:   calendar.Event{ID: item.ID, Provider: "microsoft"},
			Version: version,
			Deleted: true,
		}, nil
	}
	ev := calendar.Event{
		ID:       item.ID,
		Title:    item.Subject,
		Location: item.Location.DisplayName,
		Provider: "microsoft",
	}
	if o := item.Organizer; o != nil && (o.EmailAddress.Address != "" || o.EmailAddress.Name != "") {
		ev.Organizer = &calendar.Organizer{Email: o.EmailAddress.Address, Name: o.EmailAddress.Name}
	}
	start, err := parseGraphTime(item.Start)
	if err != nil {
		return Change{}, fmt.Errorf("%w: event %q: %v", calendar.ErrProviderBadResponse, item.ID, err)
	}
	end, err := parseGraphTime(item.End)
	if err != nil {
		return Change{}, fmt.Errorf("%w: event %q: %v", calendar.ErrProviderBadResponse, item.ID, err)
	}
	if item.IsAllDay {
		ev.AllDay = true
		ev.Start = item.Start.DateTime[:len(time.DateOnly)]
		ev.End = item.End.DateTime[:len(time.DateOnly)]
		return Change{Event: ev, Version: version}, nil
	}
	ev.Start = start.UTC().Format(time.RFC3339)
	ev.End = end.UTC().Format(time.RFC3339)
	ev.Timezone = calendar.Zone(item.OriginalStartTimeZone)
	return Change{Event: ev, Version: version}, nil
}

func parseGraphTime(dt graphWhen) (time.Time, error) {
	if len(dt.DateTime) < len(time.DateOnly) {
		return time.Time{}, fmt.Errorf("bad dateTime %q", dt.DateTime)
	}
	loc := time.UTC
	if dt.TimeZone != "" && dt.TimeZone != "UTC" {
		name := calendar.Zone(dt.TimeZone)
		if name == "" {
			return time.Time{}, fmt.Errorf("unknown timeZone %q", dt.TimeZone)
		}
		l, err := time.LoadLocation(name)
		if err != nil {
			return time.Time{}, err
		}
		loc = l
	}
	return time.ParseInLocation(graphDateTimeLayout, dt.DateTime, loc)
}
