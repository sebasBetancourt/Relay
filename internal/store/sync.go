package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSyncSuperseded: another worker committed a newer cursor for this
// account while this one was talking to the provider. The caller syncs
// once more from the cursor now stored.
var ErrSyncSuperseded = errors.New("sync cursor moved")

// SyncState is one account's cursor. Cursor is a provider secret.
type SyncState struct {
	AccountID    string
	Cursor       string
	WindowStart  time.Time
	WindowEnd    time.Time
	FullSyncedAt time.Time
	LastSyncedAt time.Time
	Status       string
	LastError    string
}

// EventChange is one upsert. Data is the normalized event JSON.
type EventChange struct {
	ProviderEventID string
	Version         string
	Data            json.RawMessage
	Deleted         bool
}

// StoredEvent is a row of synced_events.
type StoredEvent struct {
	ProviderEventID string
	Version         string
	Data            json.RawMessage
	Deleted         bool
	UpdatedAt       time.Time
}

// ApplyRequest is one successful provider round, committed as a unit.
type ApplyRequest struct {
	AccountID string
	// Expect is the cursor this round started from. The write waits on
	// the row lock and refuses to commit when the stored cursor differs.
	Expect      string
	Next        string
	Full        bool
	WindowStart time.Time
	WindowEnd   time.Time
	Changes     []EventChange
}

// SyncStore persists account_sync_state and synced_events.
type SyncStore struct {
	db *pgxpool.Pool
	// beforeCommit, when set, runs inside Apply's transaction. Tests use
	// it to prove a failure rolls the cursor back. Production leaves it nil.
	beforeCommit func() error
}

func NewSyncStore(db *pgxpool.Pool) *SyncStore {
	return &SyncStore{db: db}
}

// State returns the account's cursor. A missing row is a zero state, not
// an error: the first sync has not run.
func (s *SyncStore) State(ctx context.Context, accountID string) (SyncState, error) {
	var st SyncState
	var start, end, fullAt, lastAt *time.Time
	err := s.db.QueryRow(ctx, `
		SELECT account_id::text, cursor, window_start, window_end,
		       full_synced_at, last_synced_at, status, last_error
		FROM account_sync_state WHERE account_id = $1`, accountID,
	).Scan(&st.AccountID, &st.Cursor, &start, &end, &fullAt, &lastAt, &st.Status, &st.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncState{}, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgInvalidTextRepr {
		return SyncState{}, ErrNotFound
	}
	if err != nil {
		return SyncState{}, fmt.Errorf("sync state: %w", err)
	}
	st.WindowStart = utc(start)
	st.WindowEnd = utc(end)
	st.FullSyncedAt = utc(fullAt)
	st.LastSyncedAt = utc(lastAt)
	return st, nil
}

func utc(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// Events lists the account's synced events, including tombstones.
func (s *SyncStore) Events(ctx context.Context, accountID string) ([]StoredEvent, error) {
	rows, err := s.db.Query(ctx, `
		SELECT provider_event_id, version, data, deleted, updated_at
		FROM synced_events WHERE account_id = $1
		ORDER BY provider_event_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("synced events: %w", err)
	}
	defer rows.Close()
	var out []StoredEvent
	for rows.Next() {
		var ev StoredEvent
		if err := rows.Scan(&ev.ProviderEventID, &ev.Version, &ev.Data, &ev.Deleted, &ev.UpdatedAt); err != nil {
			return nil, fmt.Errorf("synced events: %w", err)
		}
		ev.UpdatedAt = ev.UpdatedAt.UTC()
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("synced events: %w", err)
	}
	return out, nil
}

// Apply writes changes and the new cursor in one transaction. A failure
// leaves the previous cursor in place. Unchanged versions are not rewritten.
func (s *SyncStore) Apply(ctx context.Context, req ApplyRequest) error {
	if req.Next == "" {
		return fmt.Errorf("apply sync: empty cursor")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("apply sync: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO account_sync_state (account_id) VALUES ($1)
		ON CONFLICT (account_id) DO NOTHING`, req.AccountID); err != nil {
		return fmt.Errorf("apply sync: insert: %w", err)
	}
	var cursor string
	if err := tx.QueryRow(ctx, `
		SELECT cursor FROM account_sync_state WHERE account_id = $1 FOR UPDATE`,
		req.AccountID).Scan(&cursor); err != nil {
		return fmt.Errorf("apply sync: lock: %w", err)
	}
	if cursor != req.Expect {
		return ErrSyncSuperseded
	}

	if len(req.Changes) > 0 {
		batch := &pgx.Batch{}
		for _, ch := range req.Changes {
			batch.Queue(`
				INSERT INTO synced_events (account_id, provider_event_id, version, data, deleted, updated_at)
				VALUES ($1, $2, $3, $4::jsonb, $5, now())
				ON CONFLICT (account_id, provider_event_id) DO UPDATE SET
					version = EXCLUDED.version,
					data = CASE
						WHEN EXCLUDED.deleted
						 AND COALESCE(EXCLUDED.data->>'start', '') = ''
						 AND COALESCE(EXCLUDED.data->>'title', '') = ''
						THEN synced_events.data
						ELSE EXCLUDED.data
					END,
					deleted = EXCLUDED.deleted,
					updated_at = now()
				WHERE synced_events.version IS DISTINCT FROM EXCLUDED.version
				   OR synced_events.deleted IS DISTINCT FROM EXCLUDED.deleted`,
				req.AccountID, ch.ProviderEventID, ch.Version, string(ch.Data), ch.Deleted)
		}
		br := tx.SendBatch(ctx, batch)
		for range req.Changes {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return fmt.Errorf("apply sync: upsert: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("apply sync: upsert: %w", err)
		}
	}

	if req.Full {
		ids := make([]string, 0, len(req.Changes))
		for _, ch := range req.Changes {
			if !ch.Deleted {
				ids = append(ids, ch.ProviderEventID)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE synced_events
			SET deleted = true, version = '', updated_at = now()
			WHERE account_id = $1 AND deleted = false
			  AND NOT (provider_event_id = ANY($2::text[]))`,
			req.AccountID, ids); err != nil {
			return fmt.Errorf("apply sync: tombstone: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE account_sync_state SET
			cursor = $2,
			window_start = $3,
			window_end = $4,
			last_synced_at = now(),
			status = 'ok',
			last_error = '',
			full_synced_at = CASE WHEN $5 THEN now() ELSE full_synced_at END
		WHERE account_id = $1`,
		req.AccountID, req.Next, req.WindowStart, req.WindowEnd, req.Full); err != nil {
		return fmt.Errorf("apply sync: cursor: %w", err)
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("apply sync: commit: %w", err)
	}
	return nil
}

// MarkError records a failed round without moving the cursor.
func (s *SyncStore) MarkError(ctx context.Context, accountID, message string) error {
	if len(message) > 300 {
		message = message[:300]
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO account_sync_state (account_id, status, last_error)
		VALUES ($1, 'error', $2)
		ON CONFLICT (account_id) DO UPDATE SET
			status = 'error',
			last_error = EXCLUDED.last_error`,
		accountID, message)
	if err != nil {
		return fmt.Errorf("sync error: %w", err)
	}
	return nil
}

// ClaimStale locks active accounts whose last sync is missing or older
// than age. fn must only copy ids. Publishing happens after commit.
func (s *SyncStore) ClaimStale(ctx context.Context, age time.Duration, limit int, fn func(ids []string) error) error {
	if limit <= 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("claim sync: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT a.id::text
		FROM connected_accounts a
		LEFT JOIN account_sync_state s ON s.account_id = a.id
		WHERE a.status = 'active'
		  AND (s.last_synced_at IS NULL OR s.last_synced_at < now() - make_interval(secs => $1))
		ORDER BY s.last_synced_at NULLS FIRST, a.created_at
		LIMIT $2
		FOR UPDATE OF a SKIP LOCKED`, age.Seconds(), limit)
	if err != nil {
		return fmt.Errorf("claim sync: select: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("claim sync: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("claim sync: rows: %w", err)
	}
	rows.Close()
	if fn != nil {
		if err := fn(ids); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("claim sync: commit: %w", err)
	}
	return nil
}
