package notify

import (
	"context"
	"net/http"
	"time"
)

const (
	graphAPI = "https://graph.microsoft.com"
	// graphMaxTTL is the longest Graph will schedule me/events notifications
	// (10,080 minutes). One minute is held back so a fast clock is not rejected.
	graphMaxTTL = 10080*time.Minute - time.Minute
)

// Microsoft creates, patches and deletes Graph subscriptions on me/events.
type Microsoft struct {
	BaseURL string
	HTTP    *http.Client
}

// NewMicrosoft builds a production Graph subscription client.
func NewMicrosoft(httpClient *http.Client) *Microsoft {
	return &Microsoft{BaseURL: graphAPI, HTTP: httpClient}
}

type graphCreate struct {
	ChangeType               string `json:"changeType"`
	NotificationURL          string `json:"notificationUrl"`
	LifecycleNotificationURL string `json:"lifecycleNotificationUrl"`
	Resource                 string `json:"resource"`
	ExpirationDateTime       string `json:"expirationDateTime"`
	ClientState              string `json:"clientState"`
}

type graphSub struct {
	ID                 string `json:"id"`
	ExpirationDateTime string `json:"expirationDateTime"`
}

func (m *Microsoft) Subscribe(ctx context.Context, accessToken, publicBase string) (Channel, error) {
	secret, err := newSecret()
	if err != nil {
		return Channel{}, err
	}
	exp := time.Now().UTC().Add(graphMaxTTL)
	var out graphSub
	err = callJSON(ctx, m.HTTP, http.MethodPost, m.base()+"/v1.0/subscriptions", accessToken, graphCreate{
		ChangeType:               "created,updated,deleted",
		NotificationURL:          hook(publicBase, "/v1/webhooks/microsoft"),
		LifecycleNotificationURL: hook(publicBase, "/v1/webhooks/microsoft/lifecycle"),
		Resource:                 "me/events",
		ExpirationDateTime:       exp.Format(time.RFC3339),
		ClientState:              secret,
	}, &out)
	if err != nil {
		return Channel{}, err
	}
	got := parseGraphTime(out.ExpirationDateTime, exp)
	return Channel{ExternalID: out.ID, Secret: secret, ExpiresAt: got}, nil
}

func (m *Microsoft) Renew(ctx context.Context, accessToken, _ string, current Channel) (Channel, bool, error) {
	exp := time.Now().UTC().Add(graphMaxTTL)
	var out graphSub
	err := callJSON(ctx, m.HTTP, http.MethodPatch, m.base()+"/v1.0/subscriptions/"+current.ExternalID, accessToken,
		map[string]string{"expirationDateTime": exp.Format(time.RFC3339)}, &out)
	if err != nil {
		return Channel{}, false, err
	}
	return Channel{
		ExternalID: current.ExternalID,
		ResourceID: current.ResourceID,
		ExpiresAt:  parseGraphTime(out.ExpirationDateTime, exp),
	}, false, nil
}

func (m *Microsoft) Stop(ctx context.Context, accessToken string, current Channel) error {
	err := callJSON(ctx, m.HTTP, http.MethodDelete, m.base()+"/v1.0/subscriptions/"+current.ExternalID, accessToken, nil, nil)
	if err != nil && !isGone(err) {
		return err
	}
	return nil
}

func (m *Microsoft) base() string {
	if m.BaseURL != "" {
		return m.BaseURL
	}
	return graphAPI
}

func parseGraphTime(raw string, fallback time.Time) time.Time {
	if raw == "" {
		return fallback.UTC()
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC()
	}
	return fallback.UTC()
}
