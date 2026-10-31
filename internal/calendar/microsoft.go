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
	microsoftGraphURL = "https://graph.microsoft.com"
	maxGraphBody      = 5 << 20 // 5 MiB
	graphEventsSelect = "id,changeKey,subject,start,end,isAllDay,isCancelled,location,organizer,originalStartTimeZone"
	// Graph wall-clock times: "2026-10-20T14:00:00.0000000" (no offset).
	graphDateTimeLayout = "2006-01-02T15:04:05.9999999"
)

// ErrCalendarUnavailable: the account has no usable calendar (e.g. a
// Microsoft account without an Outlook mailbox).
var ErrCalendarUnavailable = errors.New("calendar unavailable")

// MicrosoftSource lists events from Microsoft Graph (Outlook).
type MicrosoftSource struct {
	BaseURL string
	HTTP    *http.Client
}

// NewMicrosoftSource builds a production Microsoft Graph client.
func NewMicrosoftSource(httpClient *http.Client) *MicrosoftSource {
	return &MicrosoftSource{BaseURL: microsoftGraphURL, HTTP: httpClient}
}

type graphEventsResponse struct {
	Value    []graphEvent `json:"value"`
	NextLink string       `json:"@odata.nextLink"`
}

type graphEvent struct {
	ID                    string        `json:"id"`
	ChangeKey             string        `json:"changeKey"`
	Subject               string        `json:"subject"`
	IsAllDay              bool          `json:"isAllDay"`
	IsCancelled           bool          `json:"isCancelled"`
	Start                 graphDateTime `json:"start"`
	End                   graphDateTime `json:"end"`
	OriginalStartTimeZone string        `json:"originalStartTimeZone"`
	Location              struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	Organizer *struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"organizer"`
}

type graphDateTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type graphErrorBody struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// ListEvents reads /me/calendarView, which expands recurring events into
// occurrences (like Google's singleEvents=true), in the query window.
func (m *MicrosoftSource) ListEvents(ctx context.Context, accessToken string, q Query) (Page, error) {
	vals := url.Values{}
	vals.Set("startDateTime", q.TimeMin.UTC().Format(time.RFC3339))
	vals.Set("endDateTime", q.TimeMax.UTC().Format(time.RFC3339))
	vals.Set("$orderby", "start/dateTime")
	vals.Set("$select", graphEventsSelect)
	if q.MaxResults > 0 {
		vals.Set("$top", strconv.Itoa(q.MaxResults))
	}
	if q.PageToken != "" {
		key, value, err := parseGraphPageToken(q.PageToken)
		if err != nil {
			return Page{}, err
		}
		vals.Set(key, value)
	}
	// Relay builds the URL itself: the host is fixed, and only a validated
	// paging value from Graph's nextLink is reused (never the link).
	endpoint := strings.TrimRight(m.BaseURL, "/") + "/v1.0/me/calendarView?" +
		strings.ReplaceAll(vals.Encode(), "%24", "$")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Page{}, fmt.Errorf("graph list: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	// All dateTime values come back in UTC: no offset guessing.
	req.Header.Set("Prefer", `outlook.timezone="UTC"`)

	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return Page{}, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxGraphBody+1))
	if err != nil {
		return Page{}, fmt.Errorf("%w: read body: %v", ErrProviderUnavailable, err)
	}
	if len(body) > maxGraphBody {
		return Page{}, ErrProviderBadResponse
	}

	if res.StatusCode == http.StatusOK {
		return parseGraphEvents(body)
	}
	return Page{}, graphStatusError(res.StatusCode, res.Header, body)
}

// graphStatusError maps a non-success answer.
func graphStatusError(status int, header http.Header, body []byte) error {
	retryAfter := header.Get("Retry-After")
	code := graphErrorCode(body)
	switch {
	case status == http.StatusUnauthorized:
		return ErrTokenRejected
	case code == "MailboxNotEnabledForRESTAPI":
		return ErrCalendarUnavailable
	case status == http.StatusNotFound:
		return ErrCalendarNotFound
	case status == http.StatusPreconditionFailed, code == "ErrorIrresolvableConflict":
		return ErrVersionConflict
	case status == http.StatusForbidden && (code == "ErrorAccessDenied" || code == "Authorization_RequestDenied"):
		return ErrScopeRejected
	case status == http.StatusTooManyRequests,
		status == http.StatusServiceUnavailable && retryAfter != "":
		// Graph throttles with 429, and sometimes 503 + Retry-After.
		return &RateLimitedError{RetryAfter: parseRetryAfter(retryAfter)}
	case status >= 500:
		return fmt.Errorf("%w: status %d", ErrProviderUnavailable, status)
	default:
		return fmt.Errorf("%w: status %d", ErrProviderBadResponse, status)
	}
}

func parseGraphEvents(body []byte) (Page, error) {
	var raw graphEventsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return Page{}, fmt.Errorf("%w: %v", ErrProviderBadResponse, err)
	}
	events := make([]Event, 0, len(raw.Value))
	for _, item := range raw.Value {
		if item.IsCancelled {
			continue
		}
		ev, err := normalizeGraphEvent(item)
		if err != nil {
			return Page{}, fmt.Errorf("%w: event %q: %v", ErrProviderBadResponse, item.ID, err)
		}
		events = append(events, ev)
	}

	var next string
	if raw.NextLink != "" {
		token, err := graphNextToken(raw.NextLink)
		if err != nil {
			return Page{}, err
		}
		next = token
	}
	return Page{Events: events, NextPageToken: next}, nil
}

func normalizeGraphEvent(item graphEvent) (Event, error) {
	ev := Event{
		ID:       item.ID,
		Title:    item.Subject, // empty stays empty: do not invent a title
		Location: item.Location.DisplayName,
		Provider: "microsoft",
		Version:  item.ChangeKey,
	}
	if o := item.Organizer; o != nil && (o.EmailAddress.Address != "" || o.EmailAddress.Name != "") {
		ev.Organizer = &Organizer{Email: o.EmailAddress.Address, Name: o.EmailAddress.Name}
	}

	start, err := parseGraphTime(item.Start)
	if err != nil {
		return Event{}, err
	}
	end, err := parseGraphTime(item.End)
	if err != nil {
		return Event{}, err
	}

	if item.IsAllDay {
		// All-day events are midnight-to-midnight; keep the calendar date
		// only. End is exclusive, as in Google.
		ev.AllDay = true
		ev.Start = item.Start.DateTime[:len(time.DateOnly)]
		ev.End = item.End.DateTime[:len(time.DateOnly)]
		return ev, nil
	}
	ev.Start = start.UTC().Format(time.RFC3339)
	ev.End = end.UTC().Format(time.RFC3339)
	ev.Timezone = ianaZone(item.OriginalStartTimeZone)
	return ev, nil
}

// parseGraphTime reads a Graph wall-clock time in its timeZone. With the
// Prefer header that is UTC; any other zone Graph returns is honored.
func parseGraphTime(dt graphDateTime) (time.Time, error) {
	if len(dt.DateTime) < len(time.DateOnly) {
		return time.Time{}, fmt.Errorf("bad dateTime %q", dt.DateTime)
	}
	loc := time.UTC
	if dt.TimeZone != "" && dt.TimeZone != "UTC" {
		l, ok := location(dt.TimeZone)
		if !ok {
			return time.Time{}, fmt.Errorf("unknown timeZone %q", dt.TimeZone)
		}
		loc = l
	}
	return time.ParseInLocation(graphDateTimeLayout, dt.DateTime, loc)
}

// graphNextToken keeps only the paging value of @odata.nextLink. Relay
// never follows that URL: if a client could feed one back, Relay would
// call an arbitrary host with the user's token.
func graphNextToken(nextLink string) (string, error) {
	u, err := url.Parse(nextLink)
	if err != nil {
		return "", fmt.Errorf("%w: bad nextLink", ErrProviderBadResponse)
	}
	q := u.Query()
	if v := q.Get("$skiptoken"); v != "" {
		return "skiptoken:" + v, nil
	}
	if v := q.Get("$skip"); v != "" {
		if _, err := strconv.Atoi(v); err == nil {
			return "skip:" + v, nil
		}
	}
	return "", fmt.Errorf("%w: nextLink without a paging value", ErrProviderBadResponse)
}

func parseGraphPageToken(token string) (key, value string, err error) {
	kind, value, ok := strings.Cut(token, ":")
	switch {
	case ok && kind == "skiptoken" && value != "":
		return "$skiptoken", value, nil
	case ok && kind == "skip":
		if n, err := strconv.Atoi(value); err == nil && n >= 0 {
			return "$skip", value, nil
		}
	}
	return "", "", ErrInvalidPageToken
}

func graphErrorCode(body []byte) string {
	var eb graphErrorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		return ""
	}
	return eb.Error.Code
}
