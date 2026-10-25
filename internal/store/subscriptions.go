package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Subscription is one provider push registration for an account.
// SecretHash is a hex SHA-256 of the channel token or clientState.
// The plaintext secret is never stored.
type Subscription struct {
	ID         string
	AccountID  string
	Provider   string
	ExternalID string
	ResourceID string
	SecretHash string
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// Subscriptions persists provider_subscriptions.
type Subscriptions struct {
	db *pgxpool.Pool
}

func NewSubscriptions(db *pgxpool.Pool) *Subscriptions {
	return &Subscriptions{db: db}
}

// Upsert inserts or replaces the account's subscription. One account has
// one row; a Google renew changes external_id in place.
func (s *Subscriptions) Upsert(ctx context.Context, sub Subscription) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO provider_subscriptions (
			account_id, provider, external_id, resource_id, secret_hash, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (account_id) DO UPDATE SET
			provider    = EXCLUDED.provider,
			external_id = EXCLUDED.external_id,
			resource_id = EXCLUDED.resource_id,
			secret_hash = EXCLUDED.secret_hash,
			expires_at  = EXCLUDED.expires_at`,
		sub.AccountID, sub.Provider, sub.ExternalID, sub.ResourceID, sub.SecretHash, sub.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("upsert subscription: %w", err)
	}
	return nil
}

// ByExternal finds the subscription a webhook is talking about.
func (s *Subscriptions) ByExternal(ctx context.Context, provider, externalID string) (Subscription, error) {
	return s.one(ctx, `
		SELECT id::text, account_id::text, provider::text, external_id, resource_id,
		       secret_hash, expires_at, created_at
		FROM provider_subscriptions
		WHERE provider = $1 AND external_id = $2`, provider, externalID)
}

// ByAccount returns the account's subscription, or ErrNotFound.
func (s *Subscriptions) ByAccount(ctx context.Context, accountID string) (Subscription, error) {
	sub, err := s.one(ctx, `
		SELECT id::text, account_id::text, provider::text, external_id, resource_id,
		       secret_hash, expires_at, created_at
		FROM provider_subscriptions
		WHERE account_id = $1`, accountID)
	if err != nil {
		return Subscription{}, err
	}
	return sub, nil
}

func (s *Subscriptions) one(ctx context.Context, sql string, args ...any) (Subscription, error) {
	var sub Subscription
	err := s.db.QueryRow(ctx, sql, args...).Scan(
		&sub.ID, &sub.AccountID, &sub.Provider, &sub.ExternalID, &sub.ResourceID,
		&sub.SecretHash, &sub.ExpiresAt, &sub.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgInvalidTextRepr {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("subscription: %w", err)
	}
	sub.ExpiresAt = sub.ExpiresAt.UTC()
	sub.CreatedAt = sub.CreatedAt.UTC()
	return sub, nil
}

// Delete removes the account's subscription row. Missing is not an error.
func (s *Subscriptions) Delete(ctx context.Context, accountID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM provider_subscriptions WHERE account_id = $1`, accountID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgInvalidTextRepr {
			return nil
		}
		return fmt.Errorf("delete subscription: %w", err)
	}
	return nil
}

// ClaimExpiring locks active accounts whose push subscription expires within
// ahead, compared with Postgres now(). fn must only copy ids: the provider
// call happens after commit, so this lock never nests with another query.
func (s *Subscriptions) ClaimExpiring(ctx context.Context, ahead time.Duration, limit int, fn func(ids []string) error) error {
	if limit <= 0 {
		return nil
	}
	return claimIDs(ctx, s.db, fn, `
		SELECT s.account_id::text
		FROM provider_subscriptions s
		JOIN connected_accounts a ON a.id = s.account_id AND a.status = 'active'
		WHERE s.expires_at < now() + make_interval(secs => $1)
		ORDER BY s.expires_at
		LIMIT $2
		FOR UPDATE OF s SKIP LOCKED`, ahead.Seconds(), limit)
}

// ClaimMissing locks active accounts that have no subscription row yet, so
// a failed subscribe at connect is retried by one replica.
func (s *Subscriptions) ClaimMissing(ctx context.Context, limit int, fn func(ids []string) error) error {
	if limit <= 0 {
		return nil
	}
	return claimIDs(ctx, s.db, fn, `
		SELECT a.id::text
		FROM connected_accounts a
		WHERE a.status = 'active'
		  AND NOT EXISTS (
		      SELECT 1 FROM provider_subscriptions s WHERE s.account_id = a.id
		  )
		ORDER BY a.created_at
		LIMIT $1
		FOR UPDATE OF a SKIP LOCKED`, limit)
}

func claimIDs(ctx context.Context, db *pgxpool.Pool, fn func([]string) error, sql string, args ...any) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("claim subscriptions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("claim subscriptions: select: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("claim subscriptions: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("claim subscriptions: rows: %w", err)
	}
	rows.Close()

	if fn != nil {
		if err := fn(ids); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("claim subscriptions: commit: %w", err)
	}
	return nil
}
