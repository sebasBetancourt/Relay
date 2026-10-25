package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sebasBetancourt/relay/internal/store"
)

func webhookService(t *testing.T, subs *memSubs) (*Service, *memPub) {
	t.Helper()
	pub := &memPub{}
	svc := New(Deps{
		Subs: subs, Publish: pub, BaseURL: "https://relay.example",
		Providers: map[string]Provider{"google": panicProvider{}, "microsoft": panicProvider{}},
	})
	return svc, pub
}

func seedSub(subs *memSubs, provider, external, secret, account string) {
	_ = subs.Upsert(context.Background(), store.Subscription{
		AccountID: account, Provider: provider, ExternalID: external,
		ResourceID: "resource-" + external, SecretHash: HashSecret(secret),
	})
	subs.upserts = 0
}

func TestGoogleWebhook(t *testing.T) {
	subs := newMemSubs()
	seedSub(subs, "google", "ch-1", "secret-ok", "acct-1")
	svc, pub := webhookService(t, subs)

	ok := httptest.NewRequest(http.MethodPost, "/v1/webhooks/google", nil)
	ok.Header.Set("X-Goog-Channel-Id", "ch-1")
	ok.Header.Set("X-Goog-Channel-Token", "secret-ok")
	ok.Header.Set("X-Goog-Resource-State", "exists")
	ok.Header.Set("X-Goog-Message-Number", "7")
	rec := httptest.NewRecorder()
	svc.Google(rec, ok)
	if rec.Code != http.StatusAccepted || len(pub.msgs) != 1 {
		t.Fatalf("status %d msgs %d", rec.Code, len(pub.msgs))
	}
	if pub.msgs[0].subject != SubjectSync || pub.msgs[0].id != "google:ch-1:7" {
		t.Fatalf("job %+v", pub.msgs[0])
	}
	var body syncMessage
	if err := json.Unmarshal(pub.msgs[0].body, &body); err != nil || body.AccountID != "acct-1" {
		t.Fatalf("body %s", pub.msgs[0].body)
	}

	pub.msgs = nil
	syncReq := httptest.NewRequest(http.MethodPost, "/v1/webhooks/google", nil)
	syncReq.Header = ok.Header.Clone()
	syncReq.Header.Set("X-Goog-Resource-State", "sync")
	rec = httptest.NewRecorder()
	svc.Google(rec, syncReq)
	if rec.Code != http.StatusOK || len(pub.msgs) != 0 {
		t.Fatalf("sync status %d msgs %d", rec.Code, len(pub.msgs))
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/webhooks/google", nil)
	bad.Header = ok.Header.Clone()
	bad.Header.Set("X-Goog-Channel-Token", "nope")
	bad.Header.Set("X-Goog-Resource-State", "exists")
	rec = httptest.NewRecorder()
	svc.Google(rec, bad)
	if rec.Code != http.StatusUnauthorized || len(pub.msgs) != 0 || !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Fatalf("bad token status %d body %s msgs %d", rec.Code, rec.Body.String(), len(pub.msgs))
	}

	unknown := httptest.NewRequest(http.MethodPost, "/v1/webhooks/google", nil)
	unknown.Header.Set("X-Goog-Channel-Id", "missing")
	unknown.Header.Set("X-Goog-Channel-Token", "secret-ok")
	unknown.Header.Set("X-Goog-Resource-State", "exists")
	unknown.Header.Set("X-Goog-Message-Number", "1")
	rec = httptest.NewRecorder()
	svc.Google(rec, unknown)
	if rec.Code != http.StatusOK || len(pub.msgs) != 0 {
		t.Fatalf("unknown status %d msgs %d", rec.Code, len(pub.msgs))
	}
}

func TestMicrosoftWebhookBatch(t *testing.T) {
	subs := newMemSubs()
	seedSub(subs, "microsoft", "s1", "secret-ok", "acct-1")
	svc, pub := webhookService(t, subs)

	rec := postJSON(svc.Microsoft, "/v1/webhooks/microsoft", `{
		"value": [
			{"subscriptionId":"s1","clientState":"secret-ok","changeType":"updated","resource":"me/events/a"},
			{"subscriptionId":"s1","clientState":"secret-ok","changeType":"deleted","resource":"me/events/b"}
		]
	}`)
	if rec.Code != http.StatusAccepted || len(pub.msgs) != 1 {
		t.Fatalf("batch status %d msgs %d", rec.Code, len(pub.msgs))
	}
	var body syncMessage
	if err := json.Unmarshal(pub.msgs[0].body, &body); err != nil || body.AccountID != "acct-1" {
		t.Fatalf("body %s", pub.msgs[0].body)
	}

	pub.msgs = nil
	rec = postJSON(svc.Microsoft, "/v1/webhooks/microsoft", `{
		"value": [
			{"subscriptionId":"s1","clientState":"secret-ok","changeType":"updated","resource":"me/events/a"},
			{"subscriptionId":"s1","clientState":"wrong","changeType":"updated","resource":"me/events/c"}
		]
	}`)
	if rec.Code != http.StatusUnauthorized || len(pub.msgs) != 0 {
		t.Fatalf("mixed batch status %d msgs %d", rec.Code, len(pub.msgs))
	}
}

func TestMicrosoftValidationToken(t *testing.T) {
	svc, _ := webhookService(t, newMemSubs())
	token := "valid token"
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/microsoft?validationToken="+url.QueryEscape(token), nil)
	rec := httptest.NewRecorder()
	svc.Microsoft(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != token {
		t.Fatalf("status %d type %q body %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("content-type %q", rec.Header().Get("Content-Type"))
	}
}

func TestMicrosoftLifecycleEnqueues(t *testing.T) {
	subs := newMemSubs()
	seedSub(subs, "microsoft", "s1", "secret-ok", "acct-1")
	svc, pub := webhookService(t, subs)

	rec := postJSON(svc.MicrosoftLifecycle, "/v1/webhooks/microsoft/lifecycle", `{
		"value": [
			{"subscriptionId":"s1","clientState":"secret-ok","lifecycleEvent":"reauthorizationRequired"},
			{"subscriptionId":"s1","clientState":"secret-ok","lifecycleEvent":"missed"},
			{"subscriptionId":"s1","clientState":"secret-ok","lifecycleEvent":"subscriptionRemoved"}
		]
	}`)
	if rec.Code != http.StatusAccepted || len(pub.msgs) != 3 {
		t.Fatalf("status %d msgs %d", rec.Code, len(pub.msgs))
	}
	var all string
	var synced bool
	for _, m := range pub.msgs {
		all += string(m.body)
		if m.subject == SubjectSync && strings.Contains(string(m.body), `"missed"`) {
			synced = true
		}
	}
	if !strings.Contains(all, `"renew"`) || !strings.Contains(all, `"resubscribe"`) || !synced {
		t.Fatalf("jobs %+v", pub.msgs)
	}
}

func TestWebhookBodyLimitAndRate(t *testing.T) {
	svc, pub := webhookService(t, newMemSubs())
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/microsoft", bytes.NewReader(bytes.Repeat([]byte("x"), maxWebhookBody+1)))
	rec := httptest.NewRecorder()
	svc.Microsoft(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", rec.Code)
	}

	svc.perMin = 2
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/google", nil)
		r.RemoteAddr = "203.0.113.5:1234"
		svc.Google(httptest.NewRecorder(), r)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/webhooks/google", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	rec = httptest.NewRecorder()
	svc.Google(rec, r)
	if rec.Code != http.StatusTooManyRequests || len(pub.msgs) != 0 {
		t.Fatalf("rate status %d msgs %d", rec.Code, len(pub.msgs))
	}
}

func postJSON(h func(http.ResponseWriter, *http.Request), path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}
