package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sebasBetancourt/relay/internal/calendar"
	"github.com/sebasBetancourt/relay/internal/oauth"
	"github.com/sebasBetancourt/relay/internal/queue"
	"github.com/sebasBetancourt/relay/internal/store"
)

// subjectSync is the job the webhook and the periodic pass both publish.
// It must stay equal to notify.SubjectSync.
const subjectSync = "relay.sync.account"

// Accounts loads a connected account. *store.Accounts implements it.
type Accounts interface {
	AccountByID(ctx context.Context, accountID string) (store.StoredAccount, error)
}

// Tokens loads and refreshes access tokens. *oauth.Service implements it.
type Tokens interface {
	AccessToken(ctx context.Context, tenantID, accountID string) (oauth.AccessToken, error)
	RefreshAccessToken(ctx context.Context, tenantID, accountID string) (oauth.AccessToken, error)
}

// States is account_sync_state and synced_events. *store.SyncStore implements it.
type States interface {
	State(ctx context.Context, accountID string) (store.SyncState, error)
	Apply(ctx context.Context, req store.ApplyRequest) error
	MarkError(ctx context.Context, accountID, message string) error
}

// Publisher enqueues a sync job. It is the queue contract: this package does
// not import the NATS client. *natsq.Client implements it.
type Publisher = queue.Publisher

// Deps wires Service.
type Deps struct {
	Log      *slog.Logger
	Accounts Accounts
	Tokens   Tokens
	Store    States
	Sources  map[string]DeltaSource
	Publish  Publisher
	Window   time.Duration
}

// Service runs one account's delta sync and enqueues the first one.
type Service struct {
	log      *slog.Logger
	accounts Accounts
	tokens   Tokens
	store    States
	sources  map[string]DeltaSource
	publish  Publisher
	window   time.Duration
}

// New builds a Service. Window defaults to 30 days.
func New(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	window := d.Window
	if window <= 0 {
		window = DefaultWindow
	}
	sources := make(map[string]DeltaSource, len(d.Sources))
	for k, v := range d.Sources {
		sources[k] = v
	}
	return &Service{
		log: log, accounts: d.Accounts, tokens: d.Tokens, store: d.Store,
		sources: sources, publish: d.Publish, window: window,
	}
}

// Kick publishes the first sync for an account that just connected.
// The callback does not wait for the provider.
func (s *Service) Kick(ctx context.Context, accountID string) error {
	if s.publish == nil {
		return errors.New("sync queue disabled")
	}
	body, err := json.Marshal(struct {
		AccountID string `json:"account_id"`
		Reason    string `json:"reason"`
	}{accountID, "connect"})
	if err != nil {
		return err
	}
	msgID := "connect:" + accountID + ":" + fmt.Sprintf("%d", time.Now().UnixNano())
	return s.publish.Publish(ctx, subjectSync, msgID, body)
}

// HandleJob runs one relay.sync.account payload. Bad JSON is permanent:
// retrying a body that cannot be read will never succeed.
func (s *Service) HandleJob(ctx context.Context, msg queue.Message) error {
	var job struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(msg.Payload, &job); err != nil || job.AccountID == "" {
		if err == nil {
			err = errors.New("sync job missing account_id")
		}
		return queue.Permanent(err)
	}
	return s.Sync(ctx, job.AccountID)
}

// Sync applies the provider's changes for one account. Inactive accounts
// are skipped. A provider failure leaves the cursor where it was.
func (s *Service) Sync(ctx context.Context, accountID string) error {
	return s.sync(ctx, accountID, false)
}

func (s *Service) sync(ctx context.Context, accountID string, retried bool) error {
	acct, err := s.accounts.AccountByID(ctx, accountID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if acct.Status != "active" {
		return nil
	}
	state, err := s.store.State(ctx, accountID)
	if err != nil {
		return err
	}
	window, full := s.chooseWindow(state, time.Now())
	cursor := state.Cursor
	if full {
		cursor = ""
	}

	tok, err := s.tokens.AccessToken(ctx, acct.TenantID, acct.ID)
	if errors.Is(err, oauth.ErrAccountRevoked) || errors.Is(err, oauth.ErrAccountDisconnected) {
		return nil
	}
	if err != nil {
		return err
	}
	src, ok := s.sources[acct.Provider]
	if !ok {
		return fmt.Errorf("sync: no source for provider %q", acct.Provider)
	}
	access := tok.Value
	changes, next, err := s.delta(ctx, src, acct, &access, cursor, window)
	if errors.Is(err, ErrCursorExpired) && cursor != "" {
		full = true
		changes, next, err = s.delta(ctx, src, acct, &access, "", window)
	}
	if err != nil {
		_ = s.store.MarkError(ctx, accountID, safeErr(err))
		s.log.Warn("calendar sync failed", "account_id", accountID, "provider", acct.Provider, "err", safeErr(err))
		return err
	}
	req, err := applyRequest(accountID, state.Cursor, next, full, window, changes)
	if err != nil {
		return err
	}
	if err := s.store.Apply(ctx, req); err != nil {
		if errors.Is(err, store.ErrSyncSuperseded) && !retried {
			return s.sync(ctx, accountID, true)
		}
		return err
	}
	s.log.Info("calendar synced", "account_id", accountID, "provider", acct.Provider, "full", full, "changes", len(changes))
	return nil
}

func (s *Service) chooseWindow(state store.SyncState, now time.Time) (Window, bool) {
	span := s.window
	fresh := Window{Start: now.UTC(), End: now.UTC().Add(span)}
	if state.Cursor == "" || state.WindowEnd.IsZero() || !state.WindowEnd.After(now) {
		return fresh, true
	}
	got := state.WindowEnd.Sub(state.WindowStart)
	if got < 0 {
		got = -got
	}
	diff := got - span
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Minute {
		return fresh, true
	}
	return Window{Start: state.WindowStart, End: state.WindowEnd}, false
}

func (s *Service) delta(ctx context.Context, src DeltaSource, acct store.StoredAccount, access *string, cursor string, window Window) ([]Change, string, error) {
	changes, next, err := src.Delta(ctx, *access, cursor, window)
	if !errors.Is(err, calendar.ErrTokenRejected) {
		return changes, next, err
	}
	tok, err := s.tokens.RefreshAccessToken(ctx, acct.TenantID, acct.ID)
	if err != nil {
		return nil, "", err
	}
	*access = tok.Value
	return src.Delta(ctx, tok.Value, cursor, window)
}

func applyRequest(accountID, expect, next string, full bool, window Window, changes []Change) (store.ApplyRequest, error) {
	out := make([]store.EventChange, 0, len(changes))
	for _, ch := range changes {
		version := ch.Version
		if version == "" {
			version = versionOf(ch)
		}
		raw, err := json.Marshal(ch.Event)
		if err != nil {
			return store.ApplyRequest{}, err
		}
		out = append(out, store.EventChange{
			ProviderEventID: ch.Event.ID,
			Version:         version,
			Data:            raw,
			Deleted:         ch.Deleted,
		})
	}
	return store.ApplyRequest{
		AccountID: accountID, Expect: expect, Next: next, Full: full,
		WindowStart: window.Start, WindowEnd: window.End, Changes: out,
	}, nil
}

func versionOf(ch Change) string {
	body, _ := json.Marshal(struct {
		E calendar.Event
		D bool
	}{ch.Event, ch.Deleted})
	sum := sha256.Sum256(body)
	return "h:" + hex.EncodeToString(sum[:8])
}

func safeErr(err error) string {
	msg := scrubURL(err)
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
