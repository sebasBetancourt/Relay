package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// sendUpdatesAll makes Google email attendees. Its default sends nothing,
// while Graph always invites: this keeps both providers doing the same thing.
const sendUpdatesAll = "?sendUpdates=all"

func (g *GoogleSource) eventsURL() string {
	return strings.TrimRight(g.BaseURL, "/") + "/calendars/primary/events"
}

func (g *GoogleSource) eventURL(eventID string) string {
	return g.eventsURL() + "/" + url.PathEscape(eventID)
}

// CreateEvent is events.insert.
func (g *GoogleSource) CreateEvent(ctx context.Context, accessToken string, c EventChange) (Event, error) {
	status, hdr, body, err := callAPI(ctx, g.HTTP, http.MethodPost, g.eventsURL()+sendUpdatesAll, accessToken, googleBody(c, false), nil)
	if err != nil {
		return Event{}, err
	}
	if status != http.StatusOK {
		return Event{}, googleStatusError(status, hdr, body)
	}
	return parseGoogleEvent(body)
}

// EventKind reads the event: an occurrence names its series.
func (g *GoogleSource) EventKind(ctx context.Context, accessToken, eventID string) (EventKind, error) {
	status, hdr, body, err := callAPI(ctx, g.HTTP, http.MethodGet, g.eventURL(eventID), accessToken, nil, nil)
	if err != nil {
		return KindSingle, err
	}
	if status != http.StatusOK {
		return KindSingle, googleStatusError(status, hdr, body)
	}
	var item googleEvent
	if err := json.Unmarshal(body, &item); err != nil {
		return KindSingle, fmt.Errorf("%w: %v", ErrProviderBadResponse, err)
	}
	switch {
	case item.Status == "cancelled": // Google keeps deleted events readable.
		return KindSingle, ErrCalendarNotFound
	case item.RecurringEventID != "":
		return KindOccurrence, nil
	case len(item.Recurrence) > 0:
		return KindSeries, nil
	}
	return KindSingle, nil
}

// UpdateEvent is events.patch. version becomes If-Match, so Google answers 412
// if the event changed since the client read it.
func (g *GoogleSource) UpdateEvent(ctx context.Context, accessToken, eventID string, c EventChange, version string) (Event, error) {
	var headers map[string]string
	if version != "" {
		headers = map[string]string{"If-Match": `"` + version + `"`}
	}
	status, hdr, body, err := callAPI(ctx, g.HTTP, http.MethodPatch, g.eventURL(eventID)+sendUpdatesAll, accessToken, googleBody(c, true), headers)
	if err != nil {
		return Event{}, err
	}
	if status != http.StatusOK {
		return Event{}, googleStatusError(status, hdr, body)
	}
	return parseGoogleEvent(body)
}

// DeleteEvent is events.delete. On a series it removes every occurrence.
func (g *GoogleSource) DeleteEvent(ctx context.Context, accessToken, eventID string) error {
	status, hdr, body, err := callAPI(ctx, g.HTTP, http.MethodDelete, g.eventURL(eventID)+sendUpdatesAll, accessToken, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return googleStatusError(status, hdr, body)
	}
	return nil
}

func parseGoogleEvent(body []byte) (Event, error) {
	var item googleEvent
	if err := json.Unmarshal(body, &item); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrProviderBadResponse, err)
	}
	return normalizeGoogleEvent(item, ""), nil
}

// googleBody builds the JSON. patch adds explicit nulls: a patch merges into
// start and end, and Google refuses an event holding both date and dateTime.
func googleBody(c EventChange, patch bool) map[string]any {
	body := map[string]any{}
	if c.Title != nil {
		body["summary"] = *c.Title
	}
	if c.Location != nil {
		body["location"] = *c.Location
	}
	if c.Description != nil {
		body["description"] = *c.Description
	}
	if c.Attendees != nil {
		attendees := make([]map[string]string, 0, len(*c.Attendees))
		for _, a := range *c.Attendees {
			attendees = append(attendees, map[string]string{"email": a})
		}
		body["attendees"] = attendees
	}
	if s := c.schedule; s != nil {
		if s.allDay {
			body["start"] = googleDate(s.startDate, patch)
			body["end"] = googleDate(s.endDate, patch)
		} else {
			body["start"] = googleTime(s.start, s.zone, patch)
			body["end"] = googleTime(s.end, s.zone, patch)
		}
	}
	if c.rule != nil {
		body["recurrence"] = []string{c.rule.rrule()}
	}
	return body
}

func googleDate(date string, patch bool) map[string]any {
	out := map[string]any{"date": date}
	if patch {
		out["dateTime"] = nil
		out["timeZone"] = nil
	}
	return out
}

func googleTime(t time.Time, zone string, patch bool) map[string]any {
	out := map[string]any{"dateTime": t.Format(time.RFC3339), "timeZone": zone}
	if patch {
		out["date"] = nil
	}
	return out
}
