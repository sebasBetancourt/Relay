package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/store"
)

// Subjects the webhook publishes. Sync jobs are consumed by internal/sync.
// Lifecycle jobs are consumed in this process, because they renew a
// subscription rather than read the calendar.
const (
	SubjectSync      = "relay.sync.account"
	SubjectLifecycle = "relay.notify.lifecycle"
)

// Tokens is the access-token source. *oauth.Service implements it.
type Tokens interface {
	AccessToken(ctx context.Context, tenantID, accountID string) (oauth.AccessToken, error)
}

// Accounts loads a connected account. *store.Accounts implements it.
type Accounts interface {
	AccountByID(ctx context.Context, accountID string) (store.StoredAccount, error)
}

// Subscriptions is the provider_subscriptions table.
type Subscriptions interface {
	Upsert(ctx context.Context, sub store.Subscription) error
	ByExternal(ctx context.Context, provider, externalID string) (store.Subscription, error)
	ByAccount(ctx context.Context, accountID string) (store.Subscription, error)
	Delete(ctx context.Context, accountID string) error
	ClaimExpiring(ctx context.Context, ahead time.Duration, limit int, fn func(ids []string) error) error
	ClaimMissing(ctx context.Context, limit int, fn func(ids []string) error) error
}

// Publisher enqueues a job. *queue.Client implements it.
type Publisher interface {
	Publish(ctx context.Context, subject, msgID string, body []byte) error
}

// Deps wires Service. HTTP clients live on the providers.
type Deps struct {
	Log       *slog.Logger
	Accounts  Accounts
	Tokens    Tokens
	Subs      Subscriptions
	Providers map[string]Provider
	Publish   Publisher
	// BaseURL is the public origin (WEBHOOK_BASE_URL). Empty disables push.
	BaseURL string
}

// Service subscribes, renews and verifies provider push notifications.
type Service struct {
	log       *slog.Logger
	accounts  Accounts
	tokens    Tokens
	subs      Subscriptions
	providers map[string]Provider
	publish   Publisher
	baseURL   string

	perMin int
	mu     sync.Mutex
	hits   map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	n     int
}

// New builds the push service. Tokens may be set later with UseTokens when
// the OAuth service is constructed second.
func New(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{
		log: log, accounts: d.Accounts, tokens: d.Tokens, subs: d.Subs,
		providers: d.Providers, publish: d.Publish, baseURL: d.BaseURL,
		perMin: 120, hits: map[string]*rateWindow{},
	}
}

// UseTokens sets the access-token source.
func (s *Service) UseTokens(t Tokens) { s.tokens = t }

// Subscribe creates the provider subscription for an active account and
// stores its hash and expiry. A provider failure leaves no row (or the
// previous row) so the renewal worker can retry.
func (s *Service) Subscribe(ctx context.Context, accountID string) error {
	acct, p, token, err := s.ready(ctx, accountID)
	if err != nil || p == nil {
		return err
	}
	ch, err := p.Subscribe(ctx, token, s.baseURL)
	if err != nil {
		return err
	}
	if err := s.subs.Upsert(ctx, stored(acct, ch)); err != nil {
		if stopErr := p.Stop(ctx, token, ch); stopErr != nil {
			s.log.Warn("orphan subscription stop failed", "account_id", accountID, "err", stopErr)
		}
		return err
	}
	s.log.Info("calendar subscription created",
		"account_id", accountID, "provider", acct.Provider, "expires_at", ch.ExpiresAt.Format(time.RFC3339))
	return nil
}

// RenewAccount extends the account's subscription. A 5xx returns an error
// and does not delete or rewrite the row. Graph 404 creates a new subscription.
func (s *Service) RenewAccount(ctx context.Context, accountID string) error {
	sub, err := s.subs.ByAccount(ctx, accountID)
	if err != nil {
		return err
	}
	acct, p, token, err := s.ready(ctx, accountID)
	if err != nil {
		return err
	}
	if p == nil {
		if acct.Status != "" && acct.Status != "active" {
			return s.subs.Delete(ctx, accountID)
		}
		return nil
	}
	current := Channel{ExternalID: sub.ExternalID, ResourceID: sub.ResourceID, ExpiresAt: sub.ExpiresAt}
	next, replaced, err := p.Renew(ctx, token, s.baseURL, current)
	if errors.Is(err, ErrGone) {
		next, err = p.Subscribe(ctx, token, s.baseURL)
		replaced = err == nil
	}
	if err != nil {
		return err
	}
	row := stored(acct, next)
	if next.Secret == "" {
		row.SecretHash = sub.SecretHash
	}
	if err := s.subs.Upsert(ctx, row); err != nil {
		if replaced {
			_ = p.Stop(ctx, token, next)
		}
		return err
	}
	if replaced && sub.ExternalID != next.ExternalID {
		if err := p.Stop(ctx, token, current); err != nil {
			s.log.Warn("old subscription stop failed", "account_id", accountID, "provider", acct.Provider, "err", err)
		}
	}
	s.log.Info("calendar subscription renewed",
		"account_id", accountID, "provider", acct.Provider, "expires_at", next.ExpiresAt.Format(time.RFC3339))
	return nil
}

// Stop tells the provider to drop the subscription and deletes the row.
// The provider call is best effort: the row is still deleted, so a disconnect
// cannot leave a channel Relay would keep renewing.
func (s *Service) Stop(ctx context.Context, accountID string) error {
	sub, err := s.subs.ByAccount(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if acct, p, token, err := s.ready(ctx, accountID); err == nil && p != nil {
		if stopErr := p.Stop(ctx, token, Channel{ExternalID: sub.ExternalID, ResourceID: sub.ResourceID}); stopErr != nil {
			s.log.Warn("provider unsubscribe failed", "account_id", accountID, "provider", acct.Provider, "err", stopErr)
		}
	}
	if err := s.subs.Delete(ctx, accountID); err != nil {
		return err
	}
	s.log.Info("calendar subscription stopped", "account_id", accountID, "provider", sub.Provider)
	return nil
}

// HandleLifecycle runs a job the webhook enqueued: renew or create again.
func (s *Service) HandleLifecycle(ctx context.Context, accountID, action string) error {
	switch action {
	case "renew":
		err := s.RenewAccount(ctx, accountID)
		if errors.Is(err, store.ErrNotFound) {
			return s.Subscribe(ctx, accountID)
		}
		return err
	case "resubscribe":
		return s.Subscribe(ctx, accountID)
	default:
		return nil
	}
}

// ready returns the provider and an access token. A nil provider with a nil
// error means "nothing to do" (push off, account not active, provider unknown).
func (s *Service) ready(ctx context.Context, accountID string) (store.StoredAccount, Provider, string, error) {
	if s.baseURL == "" {
		return store.StoredAccount{}, nil, "", nil
	}
	acct, err := s.accounts.AccountByID(ctx, accountID)
	if err != nil {
		return store.StoredAccount{}, nil, "", err
	}
	if acct.Status != "active" {
		return acct, nil, "", nil
	}
	p, ok := s.providers[acct.Provider]
	if !ok {
		return acct, nil, "", nil
	}
	if s.tokens == nil {
		return acct, nil, "", fmt.Errorf("calendar push: tokens not configured")
	}
	tok, err := s.tokens.AccessToken(ctx, acct.TenantID, accountID)
	if err != nil {
		return acct, nil, "", err
	}
	return acct, p, tok.Value, nil
}

func stored(acct store.StoredAccount, ch Channel) store.Subscription {
	return store.Subscription{
		AccountID:  acct.ID,
		Provider:   acct.Provider,
		ExternalID: ch.ExternalID,
		ResourceID: ch.ResourceID,
		SecretHash: HashSecret(ch.Secret),
		ExpiresAt:  ch.ExpiresAt,
	}
}
