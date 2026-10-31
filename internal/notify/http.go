package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/sebasBetancourt/relay/internal/store"
)

// maxWebhookBody bounds a public request. Notifications are small; anything
// larger is refused before it is parsed.
const maxWebhookBody = 64 << 10

type syncMessage struct {
	AccountID string `json:"account_id"`
	Provider  string `json:"provider"`
	Reason    string `json:"reason,omitempty"`
}

type lifeMessage struct {
	AccountID string `json:"account_id"`
	Action    string `json:"action"`
}

type outcome struct {
	status int
	jobs   []queued
}

type queued struct {
	subject string
	msgID   string
	body    []byte
}

// Google handles POST /v1/webhooks/google. It never calls Google.
func (s *Service) Google(w http.ResponseWriter, r *http.Request) {
	if !s.allow(clientIP(r), time.Now()) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	if _, err := readLimited(w, r, maxWebhookBody); err != nil {
		return
	}
	s.finish(w, r, s.acceptGoogle(r))
}

// Microsoft handles POST /v1/webhooks/microsoft, including Graph's
// validationToken handshake. It never calls Graph.
func (s *Service) Microsoft(w http.ResponseWriter, r *http.Request) {
	if !s.allow(clientIP(r), time.Now()) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	if tok := r.URL.Query().Get("validationToken"); tok != "" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, tok)
		return
	}
	body, err := readLimited(w, r, maxWebhookBody)
	if err != nil {
		return
	}
	s.finish(w, r, s.acceptGraph(r.Context(), body, false))
}

// MicrosoftLifecycle handles POST /v1/webhooks/microsoft/lifecycle.
// reauthorizationRequired and subscriptionRemoved are enqueued; missed
// enqueues a sync. Nothing calls Graph in this request.
func (s *Service) MicrosoftLifecycle(w http.ResponseWriter, r *http.Request) {
	if !s.allow(clientIP(r), time.Now()) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	if tok := r.URL.Query().Get("validationToken"); tok != "" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, tok)
		return
	}
	body, err := readLimited(w, r, maxWebhookBody)
	if err != nil {
		return
	}
	s.finish(w, r, s.acceptGraph(r.Context(), body, true))
}

func (s *Service) acceptGoogle(r *http.Request) outcome {
	id := r.Header.Get("X-Goog-Channel-Id")
	token := r.Header.Get("X-Goog-Channel-Token")
	state := strings.ToLower(r.Header.Get("X-Goog-Resource-State"))
	number := r.Header.Get("X-Goog-Message-Number")
	if number == "" {
		number = "0"
	}
	sub, found := s.lookup(r.Context(), "google", id)
	if !found {
		_ = SecretMatches(token, HashSecret("relay-unknown-channel"))
		s.log.Warn("webhook for unknown subscription", "provider", "google", "external_id", id)
		return outcome{status: http.StatusOK}
	}
	if !SecretMatches(token, sub.SecretHash) {
		s.log.Warn("webhook rejected", "provider", "google", "reason", "bad channel token", "external_id", id)
		return outcome{status: http.StatusUnauthorized}
	}
	if state == "sync" {
		return outcome{status: http.StatusOK}
	}
	body, _ := json.Marshal(syncMessage{AccountID: sub.AccountID, Provider: "google", Reason: "notification"})
	return outcome{status: http.StatusAccepted, jobs: []queued{{
		subject: SubjectSync,
		msgID:   "google:" + id + ":" + number,
		body:    body,
	}}}
}

type graphNote struct {
	SubscriptionID string `json:"subscriptionId"`
	ClientState    string `json:"clientState"`
	ChangeType     string `json:"changeType"`
	Resource       string `json:"resource"`
	LifecycleEvent string `json:"lifecycleEvent"`
}

func (s *Service) acceptGraph(ctx context.Context, body []byte, lifecycle bool) outcome {
	var payload struct {
		Value []graphNote `json:"value"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		s.log.Warn("webhook rejected", "provider", "microsoft", "reason", "bad json")
		return outcome{status: http.StatusBadRequest}
	}
	var (
		bad  bool
		jobs []queued
		seen = map[string]bool{}
	)
	for _, n := range payload.Value {
		sub, found := s.lookup(ctx, "microsoft", n.SubscriptionID)
		if !found {
			_ = SecretMatches(n.ClientState, HashSecret("relay-unknown-channel"))
			s.log.Warn("webhook for unknown subscription", "provider", "microsoft", "external_id", n.SubscriptionID)
			continue
		}
		if !SecretMatches(n.ClientState, sub.SecretHash) {
			s.log.Warn("webhook rejected", "provider", "microsoft", "reason", "bad clientState", "external_id", n.SubscriptionID)
			bad = true
			continue
		}
		key := sub.AccountID
		if lifecycle {
			key += ":" + n.LifecycleEvent
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		jobs = append(jobs, graphJob(sub, n, lifecycle))
	}
	if bad {
		return outcome{status: http.StatusUnauthorized}
	}
	if len(jobs) == 0 {
		return outcome{status: http.StatusOK}
	}
	return outcome{status: http.StatusAccepted, jobs: jobs}
}

func graphJob(sub store.Subscription, n graphNote, lifecycle bool) queued {
	msgID := "microsoft:" + n.SubscriptionID + ":" + shortHash(n.LifecycleEvent+"\n"+n.ChangeType+"\n"+n.Resource)
	if lifecycle {
		switch n.LifecycleEvent {
		case "reauthorizationRequired":
			raw, _ := json.Marshal(lifeMessage{AccountID: sub.AccountID, Action: "renew"})
			return queued{subject: SubjectLifecycle, msgID: "lifecycle:" + n.SubscriptionID + ":renew", body: raw}
		case "subscriptionRemoved":
			raw, _ := json.Marshal(lifeMessage{AccountID: sub.AccountID, Action: "resubscribe"})
			return queued{subject: SubjectLifecycle, msgID: "lifecycle:" + n.SubscriptionID + ":resubscribe", body: raw}
		default: // missed, and anything else Graph adds later
			raw, _ := json.Marshal(syncMessage{AccountID: sub.AccountID, Provider: "microsoft", Reason: "missed"})
			return queued{subject: SubjectSync, msgID: msgID, body: raw}
		}
	}
	raw, _ := json.Marshal(syncMessage{AccountID: sub.AccountID, Provider: "microsoft", Reason: "notification"})
	return queued{subject: SubjectSync, msgID: msgID, body: raw}
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func (s *Service) lookup(ctx context.Context, provider, externalID string) (store.Subscription, bool) {
	if externalID == "" {
		return store.Subscription{}, false
	}
	sub, err := s.subs.ByExternal(ctx, provider, externalID)
	if err != nil {
		return store.Subscription{}, false
	}
	return sub, true
}

func (s *Service) finish(w http.ResponseWriter, r *http.Request, out outcome) {
	if out.status == http.StatusUnauthorized {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	for _, job := range out.jobs {
		if s.publish == nil {
			s.log.Error("webhook enqueue failed", "err", "publisher not configured")
			http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := s.publish.Publish(r.Context(), job.subject, job.msgID, job.body); err != nil {
			s.log.Error("webhook enqueue failed", "err", err, "subject", job.subject)
			http.Error(w, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	if out.status == http.StatusOK {
		// Same type as the handshake, which shares this status on Graph's routes.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.WriteHeader(out.status)
}

func (s *Service) allow(ip string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.hits) > 10000 {
		s.hits = map[string]*rateWindow{}
	}
	w := s.hits[ip]
	if w == nil || now.Sub(w.start) >= time.Minute {
		s.hits[ip] = &rateWindow{start: now, n: 1}
		return true
	}
	w.n++
	return w.n <= s.perMin
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func readLimited(w http.ResponseWriter, r *http.Request, n int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, n)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			// The live server may already have written 413. ResponseRecorder
			// does not, so set it here; a second WriteHeader is a no-op.
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return nil, err
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, err
	}
	return b, nil
}
