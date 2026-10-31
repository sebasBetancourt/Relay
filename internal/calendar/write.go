package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"
)

// wallClockLayout is a local time without an offset, as Graph takes it.
const wallClockLayout = "2006-01-02T15:04:05"

// Validation codes returned to API clients.
const (
	CodeInvalidEvent          = "invalid_event"
	CodeUnsupportedRecurrence = "unsupported_recurrence"
	CodeUnsupportedTimezone   = "unsupported_timezone"
)

var (
	// ErrVersionConflict: If-Match no longer matches the event (HTTP 412).
	ErrVersionConflict = errors.New("event version conflict")
	// ErrScopeRejected: the provider refused the token's scopes. It wraps
	// ErrProviderBadResponse so a read keeps answering provider_error.
	ErrScopeRejected = fmt.Errorf("%w: scope rejected", ErrProviderBadResponse)
)

// ValidationError is a request Relay refuses before any provider sees it.
type ValidationError struct {
	Code   string
	Detail string
}

func (e *ValidationError) Error() string { return e.Code + ": " + e.Detail }

func invalid(format string, a ...any) *ValidationError {
	return &ValidationError{Code: CodeInvalidEvent, Detail: fmt.Sprintf(format, a...)}
}

// InsufficientScopeError: the account was connected without write access.
// Reconnecting with the named feature fixes it.
type InsufficientScopeError struct {
	Provider string
	Required []string
}

func (e *InsufficientScopeError) Error() string { return "insufficient scope" }

// Recurrence is the subset of RRULE that Google and Graph both represent.
// Anything else is refused: an approximation would put meetings on the wrong day.
type Recurrence struct {
	Freq     string   `json:"freq"` // DAILY | WEEKLY | MONTHLY | YEARLY
	Interval int      `json:"interval,omitempty"`
	ByDay    []string `json:"by_day,omitempty"` // MO..SU, WEEKLY only
	Until    *string  `json:"until,omitempty"`  // YYYY-MM-DD, inclusive, in the event's zone
	Count    *int     `json:"count,omitempty"`
}

// EventInput is the body of POST /v1/calendars/{id}/events.
type EventInput struct {
	Title       string      `json:"title"`
	Location    string      `json:"location"`
	Description string      `json:"description"`
	Start       string      `json:"start"` // RFC3339 with offset, or YYYY-MM-DD if AllDay
	End         string      `json:"end"`   // exclusive for all-day events
	TimeZone    string      `json:"timezone"`
	AllDay      bool        `json:"all_day"`
	Attendees   []string    `json:"attendees"`
	Recurrence  *Recurrence `json:"recurrence"`
}

// EventPatch is the body of PATCH: a nil field stays as it is. Start and End
// travel together, because neither means anything alone across two providers.
type EventPatch struct {
	Title       *string     `json:"title"`
	Location    *string     `json:"location"`
	Description *string     `json:"description"`
	Start       *string     `json:"start"`
	End         *string     `json:"end"`
	TimeZone    *string     `json:"timezone"`
	AllDay      *bool       `json:"all_day"`
	Attendees   *[]string   `json:"attendees"`
	Recurrence  *Recurrence `json:"recurrence"`
}

// schedule is a validated time range. Timed events keep their instants in the
// event's zone, so the wall clock each provider needs is one Format away.
type schedule struct {
	allDay    bool
	startDate string // all-day
	endDate   string
	start     time.Time // timed
	end       time.Time
	zone      string // IANA
	windows   string // Windows name of zone
}

// EventChange is a validated write: nil means leave it alone. Writers only
// translate it, so both providers refuse and accept exactly the same input.
type EventChange struct {
	Title       *string
	Location    *string
	Description *string
	Attendees   *[]string
	schedule    *schedule
	rule        *recurrenceRule
}

// EventKind tells a series from its occurrences.
type EventKind int

const (
	KindSingle EventKind = iota
	KindSeries
	KindOccurrence
)

// Writer changes events on one provider. Source and Writer are separate
// interfaces so a read-only provider stays a valid Source.
type Writer interface {
	CreateEvent(ctx context.Context, accessToken string, c EventChange) (Event, error)
	EventKind(ctx context.Context, accessToken, eventID string) (EventKind, error)
	UpdateEvent(ctx context.Context, accessToken, eventID string, c EventChange, version string) (Event, error)
	DeleteEvent(ctx context.Context, accessToken, eventID string) error
}

// ChangeFromInput validates a create.
func ChangeFromInput(in EventInput) (EventChange, error) {
	sched, err := parseSchedule(in.Start, in.End, in.TimeZone, in.AllDay)
	if err != nil {
		return EventChange{}, err
	}
	c := EventChange{schedule: &sched}
	if in.Title != "" {
		c.Title = &in.Title
	}
	if in.Location != "" {
		c.Location = &in.Location
	}
	if in.Description != "" {
		c.Description = &in.Description
	}
	if len(in.Attendees) > 0 {
		attendees, err := normalizeAttendees(in.Attendees)
		if err != nil {
			return EventChange{}, err
		}
		c.Attendees = &attendees
	}
	if in.Recurrence != nil {
		rule, err := parseRecurrence(*in.Recurrence, sched)
		if err != nil {
			return EventChange{}, err
		}
		c.rule = &rule
	}
	return c, nil
}

// ChangeFromPatch validates an update.
func ChangeFromPatch(p EventPatch) (EventChange, error) {
	c := EventChange{Title: p.Title, Location: p.Location, Description: p.Description}
	scheduled := p.Start != nil || p.End != nil || p.TimeZone != nil || p.AllDay != nil
	if scheduled {
		if p.Start == nil || p.End == nil {
			return EventChange{}, invalid("start and end must be sent together")
		}
		zone := ""
		if p.TimeZone != nil {
			zone = *p.TimeZone
		}
		allDay := p.AllDay != nil && *p.AllDay
		sched, err := parseSchedule(*p.Start, *p.End, zone, allDay)
		if err != nil {
			return EventChange{}, err
		}
		c.schedule = &sched
	}
	if p.Attendees != nil {
		attendees, err := normalizeAttendees(*p.Attendees)
		if err != nil {
			return EventChange{}, err
		}
		c.Attendees = &attendees
	}
	if p.Recurrence != nil {
		if c.schedule == nil {
			return EventChange{}, &ValidationError{CodeUnsupportedRecurrence, "changing recurrence needs start and end: the pattern is checked against them"}
		}
		rule, err := parseRecurrence(*p.Recurrence, *c.schedule)
		if err != nil {
			return EventChange{}, err
		}
		c.rule = &rule
	}
	if c == (EventChange{}) {
		return EventChange{}, invalid("nothing to change")
	}
	return c, nil
}

func parseSchedule(start, end, zone string, allDay bool) (schedule, error) {
	if allDay {
		s, err1 := time.Parse(time.DateOnly, start)
		e, err2 := time.Parse(time.DateOnly, end)
		if err1 != nil || err2 != nil {
			return schedule{}, invalid("all-day start and end must be YYYY-MM-DD")
		}
		if !e.After(s) {
			return schedule{}, invalid("end must be after start (it is exclusive for all-day events)")
		}
		return schedule{allDay: true, startDate: start, endDate: end, zone: "UTC", windows: "UTC"}, nil
	}

	s, err1 := time.Parse(time.RFC3339, start)
	e, err2 := time.Parse(time.RFC3339, end)
	if err1 != nil || err2 != nil {
		return schedule{}, invalid("start and end must be RFC3339 with an offset")
	}
	if !e.After(s) {
		return schedule{}, invalid("end must be after start")
	}
	if zone == "" {
		return schedule{}, invalid("timezone is required for timed events")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return schedule{}, invalid("timezone %q is not an IANA zone", zone)
	}
	windows, ok := windowsZone(zone)
	if !ok {
		return schedule{}, &ValidationError{CodeUnsupportedTimezone, fmt.Sprintf("timezone %q has no Windows equivalent in CLDR", zone)}
	}
	return schedule{start: s.In(loc), end: e.In(loc), zone: zone, windows: windows}, nil
}

// normalizeAttendees lowercases and dedupes bare addresses. A display name is
// refused rather than dropped, since it would not reach the provider.
func normalizeAttendees(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		a = strings.ToLower(strings.TrimSpace(a))
		parsed, err := mail.ParseAddress(a)
		if err != nil || parsed.Address != a {
			return nil, invalid("attendee %q is not an email address", a)
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

// callAPI sends one provider request. It returns the raw answer: each provider
// has its own error shape, so reading it belongs to the caller.
func callAPI(ctx context.Context, client *http.Client, method, endpoint, accessToken string, body any, headers map[string]string) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("encode event: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxGoogleBody+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: read body: %v", ErrProviderUnavailable, err)
	}
	if len(raw) > maxGoogleBody {
		return 0, nil, nil, ErrProviderBadResponse
	}
	return res.StatusCode, res.Header, raw, nil
}
