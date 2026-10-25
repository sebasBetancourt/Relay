package notify

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"
)

const (
	googleCalendarAPI = "https://www.googleapis.com/calendar/v3"
	googleChannelTTL  = 7 * 24 * time.Hour
)

// Google creates and stops Calendar watch channels. There is no renew API:
// Renew starts a new channel, and the caller stops the old one.
type Google struct {
	BaseURL string
	HTTP    *http.Client
}

// NewGoogle builds a production Calendar push client.
func NewGoogle(httpClient *http.Client) *Google {
	return &Google{BaseURL: googleCalendarAPI, HTTP: httpClient}
}

type googleWatch struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Address    string `json:"address"`
	Token      string `json:"token"`
	Expiration int64  `json:"expiration"`
}

type googleChannel struct {
	ID         string   `json:"id"`
	ResourceID string   `json:"resourceId"`
	Expiration millisec `json:"expiration"`
}

// millisec accepts the milliseconds Google sends as a JSON number or string.
type millisec struct{ time.Time }

func (m *millisec) UnmarshalJSON(b []byte) error {
	if string(b) == "null" || len(b) == 0 {
		return nil
	}
	raw, err := strconv.Unquote(string(b))
	if err != nil {
		raw = string(b)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return err
	}
	m.Time = time.UnixMilli(n).UTC()
	return nil
}

func (g *Google) Subscribe(ctx context.Context, accessToken, publicBase string) (Channel, error) {
	id, err := newID()
	if err != nil {
		return Channel{}, err
	}
	secret, err := newSecret()
	if err != nil {
		return Channel{}, err
	}
	exp := time.Now().Add(googleChannelTTL).UTC()
	var out googleChannel
	err = callJSON(ctx, g.HTTP, http.MethodPost, g.base()+"/calendars/primary/events/watch", accessToken, googleWatch{
		ID: id, Type: "web_hook", Address: hook(publicBase, "/v1/webhooks/google"),
		Token: secret, Expiration: exp.UnixMilli(),
	}, &out)
	if err != nil {
		return Channel{}, err
	}
	if out.Expiration.IsZero() {
		out.Expiration.Time = exp
	}
	resource := out.ResourceID
	if out.ID == "" {
		out.ID = id
	}
	return Channel{ExternalID: out.ID, ResourceID: resource, Secret: secret, ExpiresAt: out.Expiration.UTC()}, nil
}

func (g *Google) Renew(ctx context.Context, accessToken, publicBase string, _ Channel) (Channel, bool, error) {
	ch, err := g.Subscribe(ctx, accessToken, publicBase)
	if err != nil {
		return Channel{}, false, err
	}
	return ch, true, nil
}

func (g *Google) Stop(ctx context.Context, accessToken string, current Channel) error {
	err := callJSON(ctx, g.HTTP, http.MethodPost, g.base()+"/channels/stop", accessToken, map[string]string{
		"id": current.ExternalID, "resourceId": current.ResourceID,
	}, nil)
	if err != nil && !isGone(err) {
		return err
	}
	return nil
}

func (g *Google) base() string {
	if g.BaseURL != "" {
		return g.BaseURL
	}
	return googleCalendarAPI
}

func isGone(err error) bool {
	return errors.Is(err, ErrGone)
}
