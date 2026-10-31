package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// graphWriteHeaders ask for UTC in the response, as the live read does.
var graphWriteHeaders = map[string]string{"Prefer": `outlook.timezone="UTC"`}

func (m *MicrosoftSource) eventsURL() string {
	return strings.TrimRight(m.BaseURL, "/") + "/v1.0/me/events"
}

func (m *MicrosoftSource) eventURL(eventID string) string {
	return m.eventsURL() + "/" + url.PathEscape(eventID)
}

// CreateEvent posts to /me/events. Graph invites attendees on its own.
func (m *MicrosoftSource) CreateEvent(ctx context.Context, accessToken string, c EventChange) (Event, error) {
	status, hdr, body, err := callAPI(ctx, m.HTTP, http.MethodPost, m.eventsURL(), accessToken, graphBody(c), graphWriteHeaders)
	if err != nil {
		return Event{}, err
	}
	if status != http.StatusCreated {
		return Event{}, graphStatusError(status, hdr, body)
	}
	return parseGraphEvent(body)
}

// EventKind reads the event's type. Exceptions are edited occurrences.
func (m *MicrosoftSource) EventKind(ctx context.Context, accessToken, eventID string) (EventKind, error) {
	status, hdr, body, err := callAPI(ctx, m.HTTP, http.MethodGet, m.eventURL(eventID)+"?$select=id,type", accessToken, nil, nil)
	if err != nil {
		return KindSingle, err
	}
	if status != http.StatusOK {
		return KindSingle, graphStatusError(status, hdr, body)
	}
	var item struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &item); err != nil {
		return KindSingle, fmt.Errorf("%w: %v", ErrProviderBadResponse, err)
	}
	switch item.Type {
	case "seriesMaster":
		return KindSeries, nil
	case "occurrence", "exception":
		return KindOccurrence, nil
	}
	return KindSingle, nil
}

// UpdateEvent patches /me/events/{id}. version is the changeKey; Graph
// compares If-Match with its @odata.etag, which is W/"<changeKey>".
func (m *MicrosoftSource) UpdateEvent(ctx context.Context, accessToken, eventID string, c EventChange, version string) (Event, error) {
	headers := map[string]string{"Prefer": graphWriteHeaders["Prefer"]}
	if version != "" {
		headers["If-Match"] = `W/"` + version + `"`
	}
	status, hdr, body, err := callAPI(ctx, m.HTTP, http.MethodPatch, m.eventURL(eventID), accessToken, graphBody(c), headers)
	if err != nil {
		return Event{}, err
	}
	if status != http.StatusOK {
		return Event{}, graphStatusError(status, hdr, body)
	}
	return parseGraphEvent(body)
}

// DeleteEvent removes the event. On a series master it removes every occurrence.
func (m *MicrosoftSource) DeleteEvent(ctx context.Context, accessToken, eventID string) error {
	status, hdr, body, err := callAPI(ctx, m.HTTP, http.MethodDelete, m.eventURL(eventID), accessToken, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return graphStatusError(status, hdr, body)
	}
	return nil
}

func parseGraphEvent(body []byte) (Event, error) {
	var item graphEvent
	if err := json.Unmarshal(body, &item); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrProviderBadResponse, err)
	}
	ev, err := normalizeGraphEvent(item)
	if err != nil {
		return Event{}, fmt.Errorf("%w: event %q: %v", ErrProviderBadResponse, item.ID, err)
	}
	return ev, nil
}

func graphBody(c EventChange) map[string]any {
	body := map[string]any{}
	if c.Title != nil {
		body["subject"] = *c.Title
	}
	if c.Location != nil {
		body["location"] = map[string]string{"displayName": *c.Location}
	}
	if c.Description != nil {
		body["body"] = map[string]string{"contentType": "text", "content": *c.Description}
	}
	if c.Attendees != nil {
		attendees := make([]map[string]any, 0, len(*c.Attendees))
		for _, a := range *c.Attendees {
			attendees = append(attendees, map[string]any{
				"emailAddress": map[string]string{"address": a},
				"type":         "required",
			})
		}
		body["attendees"] = attendees
	}
	if s := c.schedule; s != nil {
		body["isAllDay"] = s.allDay
		if s.allDay {
			// Graph requires midnight on both ends of an all-day event.
			body["start"] = graphDateTime{DateTime: s.startDate + "T00:00:00", TimeZone: "UTC"}
			body["end"] = graphDateTime{DateTime: s.endDate + "T00:00:00", TimeZone: "UTC"}
		} else {
			body["start"] = graphDateTime{DateTime: s.start.Format(wallClockLayout), TimeZone: s.windows}
			body["end"] = graphDateTime{DateTime: s.end.Format(wallClockLayout), TimeZone: s.windows}
		}
	}
	if c.rule != nil {
		body["recurrence"] = c.rule.patternedRecurrence()
	}
	return body
}
