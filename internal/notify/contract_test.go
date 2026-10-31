package notify

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebasBetancourt/relay/internal/apikey"
	"github.com/sebasBetancourt/relay/internal/httpapi"
	"github.com/sebasBetancourt/relay/internal/testutil/contract"
)

func webhookRouter(t *testing.T, svc *Service) http.Handler {
	t.Helper()
	spec, err := contract.Default()
	if err != nil {
		t.Fatal(err)
	}
	return spec.Wrap(t, httpapi.NewRouter(httpapi.Deps{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Env: apikey.EnvTest, Webhooks: svc,
	}))
}

func postWebhook(h http.Handler, path, body string, hdr map[string]string) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func graphBody(subscription, secret string) string {
	return fmt.Sprintf(`{"value":[{"subscriptionId":%q,"clientState":%q,"changeType":"updated","resource":"me/events/1","lifecycleEvent":"missed"}]}`, subscription, secret)
}

// Every documented webhook answer, through the real router, under the contract.
// The handlers run behind a public URL, so providers depend on these shapes.
func TestWebhookResponsesMatchTheContract(t *testing.T) {
	googleHdr := func(channel, token string) map[string]string {
		return map[string]string{
			"X-Goog-Channel-ID": channel, "X-Goog-Channel-Token": token,
			"X-Goog-Resource-State": "exists", "X-Goog-Message-Number": "1",
		}
	}
	tooBig := strings.Repeat("x", maxWebhookBody+1)

	tests := []struct {
		name string
		path string
		body string
		hdr  map[string]string
		want int
	}{
		{"google enqueued", "/v1/webhooks/google", "", googleHdr("ch-1", "ok"), http.StatusAccepted},
		{"google unknown channel", "/v1/webhooks/google", "", googleHdr("nope", "ok"), http.StatusOK},
		{"google wrong secret", "/v1/webhooks/google", "", googleHdr("ch-1", "bad"), http.StatusUnauthorized},
		{"google too large", "/v1/webhooks/google", tooBig, googleHdr("ch-1", "ok"), http.StatusRequestEntityTooLarge},
		{"graph handshake", "/v1/webhooks/microsoft?validationToken=abc", "", nil, http.StatusOK},
		{"graph enqueued", "/v1/webhooks/microsoft", graphBody("sub-1", "ok"), nil, http.StatusAccepted},
		{"graph unknown subscription", "/v1/webhooks/microsoft", graphBody("nope", "ok"), nil, http.StatusOK},
		{"graph wrong secret", "/v1/webhooks/microsoft", graphBody("sub-1", "bad"), nil, http.StatusUnauthorized},
		{"graph not json", "/v1/webhooks/microsoft", "nope", nil, http.StatusBadRequest},
		{"graph too large", "/v1/webhooks/microsoft", tooBig, nil, http.StatusRequestEntityTooLarge},
		{"lifecycle handshake", "/v1/webhooks/microsoft/lifecycle?validationToken=abc", "", nil, http.StatusOK},
		{"lifecycle enqueued", "/v1/webhooks/microsoft/lifecycle", graphBody("sub-1", "ok"), nil, http.StatusAccepted},
		{"lifecycle unknown subscription", "/v1/webhooks/microsoft/lifecycle", graphBody("nope", "ok"), nil, http.StatusOK},
		{"lifecycle wrong secret", "/v1/webhooks/microsoft/lifecycle", graphBody("sub-1", "bad"), nil, http.StatusUnauthorized},
		{"lifecycle not json", "/v1/webhooks/microsoft/lifecycle", "nope", nil, http.StatusBadRequest},
		{"lifecycle too large", "/v1/webhooks/microsoft/lifecycle", tooBig, nil, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subs := newMemSubs()
			seedSub(subs, "google", "ch-1", "ok", "acct-1")
			seedSub(subs, "microsoft", "sub-1", "ok", "acct-2")
			svc, _ := webhookService(t, subs)
			if got := postWebhook(webhookRouter(t, svc), tt.path, tt.body, tt.hdr); got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWebhookFailuresMatchTheContract(t *testing.T) {
	calls := []struct {
		path string
		body string
		hdr  map[string]string
	}{
		{"/v1/webhooks/google", "", map[string]string{
			"X-Goog-Channel-ID": "ch-1", "X-Goog-Channel-Token": "ok", "X-Goog-Resource-State": "exists",
		}},
		{"/v1/webhooks/microsoft", graphBody("sub-1", "ok"), nil},
		{"/v1/webhooks/microsoft/lifecycle", graphBody("sub-1", "ok"), nil},
	}
	for _, c := range calls {
		t.Run(c.path, func(t *testing.T) {
			subs := newMemSubs()
			seedSub(subs, "google", "ch-1", "ok", "acct-1")
			seedSub(subs, "microsoft", "sub-1", "ok", "acct-2")

			svc, pub := webhookService(t, subs)
			pub.err = errors.New("queue down")
			if got := postWebhook(webhookRouter(t, svc), c.path, c.body, c.hdr); got != http.StatusServiceUnavailable {
				t.Fatalf("queue down: status = %d", got)
			}

			throttled, _ := webhookService(t, subs)
			throttled.perMin = 0
			if got := postWebhook(webhookRouter(t, throttled), c.path, c.body, c.hdr); got != http.StatusAccepted {
				t.Fatalf("first request: status = %d", got)
			}
			if got := postWebhook(webhookRouter(t, throttled), c.path, c.body, c.hdr); got != http.StatusTooManyRequests {
				t.Fatalf("second request: status = %d", got)
			}
		})
	}
}
