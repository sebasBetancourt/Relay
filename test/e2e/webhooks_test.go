package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sebasBetancourt/relay/internal/notify"
	"github.com/sebasBetancourt/relay/internal/queue/natsq"
	"github.com/sebasBetancourt/relay/internal/testutil/natsjs"
)

// enablePush rebuilds the router with subscriptions turned on. The httptest
// server is the public origin the Microsoft fake calls back during the
// validation handshake.
func (f *flow) enablePush(t *testing.T) {
	t.Helper()
	c, err := natsq.Connect(f.ctx, natsjs.Start(t), natsq.WithMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	f.nats = c

	var current atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _ := current.Load().(http.Handler)
		if h == nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.webhookBase = srv.URL
	f.router = f.buildRouter(f.pool)
	current.Store(f.router)
}

func TestConnectWithoutPushCreatesNoSubscription(t *testing.T) {
	f := newFlow(t)
	id := f.connectThrough("google")
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM provider_subscriptions WHERE account_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("subscriptions = %d, want 0 without WEBHOOK_BASE_URL", n)
	}
}

func TestPushSubscribeNotifyDisconnect(t *testing.T) {
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			f := newFlow(t)
			f.enablePush(t)
			accountID := f.connectThrough(provider)

			var expires time.Time
			err := f.pool.QueryRow(f.ctx, `
				SELECT expires_at FROM provider_subscriptions WHERE account_id = $1 AND provider = $2`,
				accountID, provider).Scan(&expires)
			if err != nil {
				t.Fatal(err)
			}
			if !expires.After(time.Now()) {
				t.Fatal("expires_at must be in the future")
			}

			rec := f.notify(provider, "1")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("notify %d %s", rec.Code, rec.Body.String())
			}
			again := f.notify(provider, "1")
			if again.Code != http.StatusAccepted {
				t.Fatalf("duplicate notify %d", again.Code)
			}
			n, err := f.nats.Pending(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("queued messages = %d, want 1", n)
			}
			msgs, err := f.nats.Collect(f.ctx, notify.SubjectSync, 5, 300*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 || !strings.Contains(string(msgs[0]), accountID) {
				t.Fatalf("messages %s", msgs)
			}
			if n, _ = f.nats.Pending(f.ctx); n != 0 {
				t.Fatalf("acked job still queued, pending %d", n)
			}

			wrong := f.postWebhook(provider, "wrong-secret", "3")
			if wrong.Code != http.StatusUnauthorized {
				t.Fatalf("bad secret %d %s", wrong.Code, wrong.Body.String())
			}
			if n, _ = f.nats.Pending(f.ctx); n != 0 {
				t.Fatalf("bad secret queued a message, pending %d", n)
			}

			rec = f.do(http.MethodDelete, "/v1/accounts/"+accountID, f.authHdr())
			if rec.Code != http.StatusNoContent {
				t.Fatalf("disconnect %d %s", rec.Code, rec.Body.String())
			}
			var left int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM provider_subscriptions WHERE account_id = $1`, accountID).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != 0 {
				t.Fatal("subscription row must be deleted")
			}
			switch provider {
			case "google":
				if f.google.Stops() != 1 {
					t.Fatalf("stops %d", f.google.Stops())
				}
			default:
				if f.ms.Deletes() != 1 {
					t.Fatalf("deletes %d", f.ms.Deletes())
				}
			}
		})
	}
}

func (f *flow) notify(provider, number string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.postWebhook(provider, f.channelSecret(provider), number)
}

func (f *flow) channelSecret(provider string) string {
	f.t.Helper()
	if provider == "microsoft" {
		return f.ms.LastSubscription().ClientState
	}
	return f.google.LastWatch().Token
}

func (f *flow) postWebhook(provider, secret, number string) *httptest.ResponseRecorder {
	f.t.Helper()
	var (
		path = "/v1/webhooks/google"
		body string
		hdr  = map[string]string{}
	)
	if provider == "microsoft" {
		path = "/v1/webhooks/microsoft"
		raw, _ := json.Marshal(map[string]any{
			"value": []map[string]string{{
				"subscriptionId": f.ms.LastSubscription().ID,
				"clientState":    secret,
				"changeType":     "updated",
				"resource":       "me/events/" + number,
			}},
		})
		body = string(raw)
	} else {
		hdr["X-Goog-Channel-Id"] = f.google.LastWatch().ID
		hdr["X-Goog-Channel-Token"] = secret
		hdr["X-Goog-Resource-Id"] = f.google.LastWatch().ResourceID
		hdr["X-Goog-Resource-State"] = "exists"
		hdr["X-Goog-Message-Number"] = number
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}
