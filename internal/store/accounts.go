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

// ErrAccountTaken is returned when provider_account_id is already linked to
// a different tenant. The row is left untouched.
var ErrAccountTaken = errors.New("connected account belongs to another tenant")

// Connection is the already-encrypted payload for one provider account.
// The store never sees plaintext tokens or the vault key.
type Connection struct {
	TenantID          string
	Provider          string
	ProviderAccountID string
	Email             string // optional; empty keeps the stored one on reconnect
	AccessEnc         []byte
	AccessNonce       []byte
	RefreshEnc        []byte // nil when the provider did not return a refresh token
	RefreshNonce      []byte
	ExpiresAt         time.Time
	Scopes            []string
}

// StoredAccount is a connected account plus its encrypted tokens (if any).
// Tokens are nil when the account was revoked and oauth_tokens was deleted.
type StoredAccount struct {
	ID                string
	TenantID          string
	Provider          string
	ProviderAccountID string
	Email             string
	Status            string
	AccessEnc         []byte
	AccessNonce       []byte
	RefreshEnc        []byte
	RefreshNonce      []byte
	ExpiresAt         time.Time // zero when no token row
	HasTokens         bool
}

// TokenUpdate is an encrypted access (and optional refresh) replacement.
type TokenUpdate struct {
	AccessEnc    []byte
	AccessNonce  []byte
	RefreshEnc   []byte // nil → keep previous refresh + nonce
	RefreshNonce []byte
	ExpiresAt    time.Time
}

type Accounts struct {
	db *pgxpool.Pool
}

func NewAccounts(db *pgxpool.Pool) *Accounts {
	return &Accounts{db: db}
}

// AccountForTenant loads an account only if it belongs to tenantID.
// Missing, wrong-tenant, and non-uuid ids all return ErrNotFound (no leak).
func (s *Accounts) AccountForTenant(ctx context.Context, accountID, tenantID string) (StoredAccount, error) {
	var a StoredAccount
	var expiresAt *time.Time
	err := s.db.QueryRow(ctx, `
		SELECT a.id::text, a.tenant_id::text, a.provider::text, a.provider_account_id,
		       COALESCE(a.email, ''), a.status::text,
		       t.access_token_enc, t.access_nonce, t.refresh_token_enc, t.refresh_nonce, t.expires_at
		FROM connected_accounts a
		LEFT JOIN oauth_tokens t ON t.account_id = a.id
		WHERE a.id = $1 AND a.tenant_id = $2`,
		accountID, tenantID,
	).Scan(
		&a.ID, &a.TenantID, &a.Provider, &a.ProviderAccountID, &a.Email, &a.Status,
		&a.AccessEnc, &a.AccessNonce, &a.RefreshEnc, &a.RefreshNonce, &expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredAccount{}, ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgInvalidTextRepr {
		return StoredAccount{}, ErrNotFound
	}
	if err != nil {
		return StoredAccount{}, fmt.Errorf("account for tenant: %w", err)
	}
	if expiresAt != nil {
		a.ExpiresAt = expiresAt.UTC()
		a.HasTokens = true
	}
	return a, nil
}

// AccountByID loads an account without a tenant check. The background
// refresher uses it; request handlers must keep using AccountForTenant.
// A non-uuid id is ErrNotFound.
func (s *Accounts) AccountByID(ctx context.Context, accountID string) (StoredAccount, error) {
	var a StoredAccount
	var expiresAt *time.Time
	err := s.db.QueryRow(ctx, `
		SELECT a.id::text, a.tenant_id::text, a.provider::text, a.provider_account_id,
		       COALESCE(a.email, ''), a.status::text,
		       t.access_token_enc, t.access_nonce, t.refresh_token_enc, t.refresh_nonce, t.expires_at
		FROM connected_accounts a
		LEFT JOIN oauth_tokens t ON t.account_id = a.id
		WHERE a.id = $1`,
		accountID,
	).Scan(
		&a.ID, &a.TenantID, &a.Provider, &a.ProviderAccountID, &a.Email, &a.Status,
		&a.AccessEnc, &a.AccessNonce, &a.RefreshEnc, &a.RefreshNonce, &expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredAccount{}, ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgInvalidTextRepr {
		return StoredAccount{}, ErrNotFound
	}
	if err != nil {
		return StoredAccount{}, fmt.Errorf("account by id: %w", err)
	}
	if expiresAt != nil {
		a.ExpiresAt = expiresAt.UTC()
		a.HasTokens = true
	}
	return a, nil
}

// ClaimExpiring locks up to limit active accounts whose access token expires
// within ahead (compared with Postgres now(), not the process clock) and
// calls fn while those rows are held with FOR UPDATE SKIP LOCKED.
//
// fn must return quickly and must not call RefreshLocked on the same rows:
// that lock is already held here, so a second one would deadlock. The
// refresher copies the ids and refreshes after this transaction commits.
// A concurrent claim skips the locked rows, so two replicas do not take
// the same account in one cycle.
func (s *Accounts) ClaimExpiring(ctx context.Context, ahead time.Duration, limit int, fn func(ids []string) error) error {
	if limit <= 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("claim expiring: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT t.account_id::text
		FROM oauth_tokens t
		JOIN connected_accounts a ON a.id = t.account_id AND a.status = 'active'
		WHERE t.expires_at < now() + make_interval(secs => $1)
		ORDER BY t.expires_at
		LIMIT $2
		FOR UPDATE OF t SKIP LOCKED`,
		ahead.Seconds(), limit,
	)
	if err != nil {
		return fmt.Errorf("claim expiring: select: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("claim expiring: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("claim expiring: rows: %w", err)
	}
	rows.Close()

	if fn != nil {
		if err := fn(ids); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("claim expiring: commit: %w", err)
	}
	return nil
}

// UpdateTokens replaces the access token (and refresh when provided).
func (s *Accounts) UpdateTokens(ctx context.Context, accountID string, u TokenUpdate) error {
	return updateTokens(ctx, s.db, accountID, u)
}

// execer is what pgxpool.Pool and pgx.Tx have in common for writes.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func updateTokens(ctx context.Context, db execer, accountID string, u TokenUpdate) error {
	tag, err := db.Exec(ctx, `
		UPDATE oauth_tokens SET
			access_token_enc  = $2,
			access_nonce      = $3,
			refresh_token_enc = CASE
				WHEN $4::bytea IS NOT NULL THEN $4
				ELSE refresh_token_enc
			END,
			refresh_nonce = CASE
				WHEN $4::bytea IS NOT NULL THEN $5
				ELSE refresh_nonce
			END,
			expires_at = $6,
			updated_at = now()
		WHERE account_id = $1`,
		accountID, u.AccessEnc, u.AccessNonce, u.RefreshEnc, u.RefreshNonce, u.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("update tokens: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RefreshLocked serializes token refreshes for one account across every API
// replica. It locks the account's oauth_tokens row (SELECT … FOR UPDATE),
// re-reads it, and calls fn with that fresh copy: if another replica
// refreshed while we waited, fn sees the new token and can return a nil
// update. A non-nil update is written in the same transaction, so a
// rotated refresh token (Microsoft) is stored before the lock is released.
//
// fn runs while the row lock and a pool connection are held, so it must be
// bounded (the provider call uses an HTTP client with a timeout) and must
// not write the same account through another connection (e.g. MarkRevoked):
// return an error instead and act after RefreshLocked returns.
//
// The account must be active and still have tokens, else ErrNotFound.
func (s *Accounts) RefreshLocked(ctx context.Context, accountID string, fn func(cur StoredAccount) (*TokenUpdate, error)) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("refresh locked: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cur StoredAccount
	err = tx.QueryRow(ctx, `
		SELECT a.id::text, a.tenant_id::text, a.provider::text, a.provider_account_id,
		       COALESCE(a.email, ''), a.status::text,
		       t.access_token_enc, t.access_nonce, t.refresh_token_enc, t.refresh_nonce, t.expires_at
		FROM oauth_tokens t
		JOIN connected_accounts a ON a.id = t.account_id
		WHERE t.account_id = $1 AND a.status = 'active'
		FOR UPDATE OF t`,
		accountID,
	).Scan(
		&cur.ID, &cur.TenantID, &cur.Provider, &cur.ProviderAccountID, &cur.Email, &cur.Status,
		&cur.AccessEnc, &cur.AccessNonce, &cur.RefreshEnc, &cur.RefreshNonce, &cur.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("refresh locked: select: %w", err)
	}
	cur.ExpiresAt = cur.ExpiresAt.UTC()
	cur.HasTokens = true

	update, err := fn(cur)
	if err != nil {
		return err
	}
	if update != nil {
		if err := updateTokens(ctx, tx, accountID, *update); err != nil {
			return fmt.Errorf("refresh locked: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("refresh locked: commit: %w", err)
	}
	return nil
}

// MarkRevoked sets status=revoked and deletes oauth_tokens in one transaction.
func (s *Accounts) MarkRevoked(ctx context.Context, accountID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mark revoked: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE connected_accounts
		SET status = 'revoked', updated_at = now()
		WHERE id = $1`, accountID)
	if err != nil {
		return fmt.Errorf("mark revoked: status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM oauth_tokens WHERE account_id = $1`, accountID); err != nil {
		return fmt.Errorf("mark revoked: tokens: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("mark revoked: commit: %w", err)
	}
	return nil
}

// SaveConnection upserts connected_accounts + oauth_tokens in one transaction.
// Same tenant reconnecting keeps the same account id and replaces tokens.
// Another tenant claiming the same provider account → ErrAccountTaken.
//
// When RefreshEnc is nil on reconnect, the previous refresh ciphertext and
// its nonce are kept together (a ciphertext without its nonce is useless).
func (s *Accounts) SaveConnection(ctx context.Context, c Connection) (accountID string, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("save connection: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO connected_accounts (tenant_id, provider, provider_account_id, email)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (provider, provider_account_id) DO UPDATE
			SET status = 'active',
			    email = COALESCE(EXCLUDED.email, connected_accounts.email),
			    updated_at = now()
			WHERE connected_accounts.tenant_id = EXCLUDED.tenant_id
		RETURNING id::text`,
		c.TenantID, c.Provider, c.ProviderAccountID, c.Email,
	).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrAccountTaken
	}
	if err != nil {
		return "", fmt.Errorf("save connection: account: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO oauth_tokens (
			account_id, access_token_enc, access_nonce,
			refresh_token_enc, refresh_nonce, expires_at, scopes
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (account_id) DO UPDATE SET
			access_token_enc  = EXCLUDED.access_token_enc,
			access_nonce      = EXCLUDED.access_nonce,
			refresh_token_enc = CASE
				WHEN EXCLUDED.refresh_token_enc IS NOT NULL THEN EXCLUDED.refresh_token_enc
				ELSE oauth_tokens.refresh_token_enc
			END,
			refresh_nonce = CASE
				WHEN EXCLUDED.refresh_token_enc IS NOT NULL THEN EXCLUDED.refresh_nonce
				ELSE oauth_tokens.refresh_nonce
			END,
			expires_at = EXCLUDED.expires_at,
			scopes     = EXCLUDED.scopes,
			updated_at = now()`,
		accountID, c.AccessEnc, c.AccessNonce, c.RefreshEnc, c.RefreshNonce, c.ExpiresAt, c.Scopes,
	)
	if err != nil {
		return "", fmt.Errorf("save connection: tokens: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("save connection: commit: %w", err)
	}
	return accountID, nil
}

// AccountSummary is what a tenant may see about a connected account:
// never tokens.
type AccountSummary struct {
	ID          string
	Provider    string
	Email       string
	Status      string
	ConnectedAt time.Time
}

// AccountCursor is the keyset position after the last listed account.
type AccountCursor struct {
	CreatedAt time.Time
	ID        string
}

// ListForTenant returns up to limit accounts of tenantID ordered by
// (created_at, id), starting after the cursor (nil = first page). Keyset
// paging stays fast and stable while accounts are added or removed, unlike
// OFFSET. Served by idx_connected_accounts_tenant_created.
func (s *Accounts) ListForTenant(ctx context.Context, tenantID string, limit int, after *AccountCursor) ([]AccountSummary, error) {
	var afterAt *time.Time
	var afterID *string
	if after != nil {
		afterAt, afterID = &after.CreatedAt, &after.ID
	}
	rows, err := s.db.Query(ctx, `
		SELECT id::text, provider::text, COALESCE(email, ''), status::text, created_at
		FROM connected_accounts
		WHERE tenant_id = $1
		  AND ($2::timestamptz IS NULL OR (created_at, id) > ($2, $3::uuid))
		ORDER BY created_at, id
		LIMIT $4`,
		tenantID, afterAt, afterID, limit,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgInvalidTextRepr {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AccountSummary, error) {
		var a AccountSummary
		err := row.Scan(&a.ID, &a.Provider, &a.Email, &a.Status, &a.ConnectedAt)
		a.ConnectedAt = a.ConnectedAt.UTC()
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return out, nil
}

// Disconnect marks the tenant's account as disconnected and deletes its
// tokens in one transaction, returning the account as it was (tokens
// included) so the caller can revoke them at the provider afterwards.
// Missing, wrong-tenant and non-uuid ids → ErrNotFound. Disconnecting an
// already disconnected account succeeds (the returned Status says so).
//
// A refresh in flight holds the token row lock; the DELETE waits for it,
// so the refresh cannot write tokens back after the disconnect.
func (s *Accounts) Disconnect(ctx context.Context, accountID, tenantID string) (StoredAccount, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return StoredAccount{}, fmt.Errorf("disconnect: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		prev      StoredAccount
		expiresAt *time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT a.id::text, a.tenant_id::text, a.provider::text, a.provider_account_id,
		       COALESCE(a.email, ''), a.status::text,
		       t.access_token_enc, t.access_nonce, t.refresh_token_enc, t.refresh_nonce, t.expires_at
		FROM connected_accounts a
		LEFT JOIN oauth_tokens t ON t.account_id = a.id
		WHERE a.id = $1 AND a.tenant_id = $2
		FOR UPDATE OF a`,
		accountID, tenantID,
	).Scan(
		&prev.ID, &prev.TenantID, &prev.Provider, &prev.ProviderAccountID, &prev.Email, &prev.Status,
		&prev.AccessEnc, &prev.AccessNonce, &prev.RefreshEnc, &prev.RefreshNonce, &expiresAt,
	)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows), errors.As(err, &pgErr) && pgErr.Code == pgInvalidTextRepr:
		return StoredAccount{}, ErrNotFound
	case err != nil:
		return StoredAccount{}, fmt.Errorf("disconnect: select: %w", err)
	}
	if expiresAt != nil {
		prev.ExpiresAt = expiresAt.UTC()
		prev.HasTokens = true
	}

	if _, err := tx.Exec(ctx, `
		UPDATE connected_accounts SET status = 'disconnected', updated_at = now()
		WHERE id = $1`, accountID); err != nil {
		return StoredAccount{}, fmt.Errorf("disconnect: status: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM oauth_tokens WHERE account_id = $1`, accountID); err != nil {
		return StoredAccount{}, fmt.Errorf("disconnect: tokens: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return StoredAccount{}, fmt.Errorf("disconnect: commit: %w", err)
	}
	return prev, nil
}
