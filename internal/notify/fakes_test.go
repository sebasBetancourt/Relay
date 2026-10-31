package notify

import (
	"context"
	"sync"
	"time"

	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

type memSubs struct {
	mu      sync.Mutex
	rows    map[string]store.Subscription // external id
	upserts int
	deletes int
}

func newMemSubs() *memSubs {
	return &memSubs{rows: map[string]store.Subscription{}}
}

func (m *memSubs) Upsert(_ context.Context, sub store.Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upserts++
	for k, existing := range m.rows {
		if existing.AccountID == sub.AccountID {
			delete(m.rows, k)
		}
	}
	m.rows[sub.Provider+":"+sub.ExternalID] = sub
	return nil
}

func (m *memSubs) ByExternal(_ context.Context, provider, externalID string) (store.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sub, ok := m.rows[provider+":"+externalID]
	if !ok {
		return store.Subscription{}, store.ErrNotFound
	}
	return sub, nil
}

func (m *memSubs) ByAccount(_ context.Context, accountID string) (store.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sub := range m.rows {
		if sub.AccountID == accountID {
			return sub, nil
		}
	}
	return store.Subscription{}, store.ErrNotFound
}

func (m *memSubs) Delete(_ context.Context, accountID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	for k, sub := range m.rows {
		if sub.AccountID == accountID {
			delete(m.rows, k)
		}
	}
	return nil
}

func (m *memSubs) ClaimExpiring(context.Context, time.Duration, int, func([]string) error) error {
	return nil
}

func (m *memSubs) ClaimMissing(context.Context, int, func([]string) error) error { return nil }

type memAcct struct{ a store.StoredAccount }

func (m memAcct) AccountByID(context.Context, string) (store.StoredAccount, error) {
	return m.a, nil
}

type memTok struct{ value string }

func (m memTok) AccessToken(context.Context, string, string) (oauth.AccessToken, error) {
	return oauth.AccessToken{Value: m.value}, nil
}

type memPub struct {
	mu   sync.Mutex
	msgs []pubMsg
	err  error
}

type pubMsg struct {
	subject string
	id      string
	body    []byte
}

func (m *memPub) Publish(_ context.Context, subject, msgID string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, pubMsg{subject: subject, id: msgID, body: append([]byte(nil), body...)})
	return m.err
}

type panicProvider struct{}

func (panicProvider) Subscribe(context.Context, string, string) (Channel, error) {
	panic("provider called from webhook")
}
func (panicProvider) Renew(context.Context, string, string, Channel) (Channel, bool, error) {
	panic("provider called from webhook")
}
func (panicProvider) Stop(context.Context, string, Channel) error {
	panic("provider called from webhook")
}
