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
		SELECT a.id::text, a.tenant_id::text, a.provider::text, a.provider_account_id, a.status::text,
		       t.access_token_enc, t.access_nonce, t.refresh_token_enc, t.refresh_nonce, t.expires_at
		FROM connected_accounts a
		LEFT JOIN oauth_tokens t ON t.account_id = a.id
		WHERE a.id = $1 AND a.tenant_id = $2`,
		accountID, tenantID,
	).Scan(
		&a.ID, &a.TenantID, &a.Provider, &a.ProviderAccountID, &a.Status,
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

// UpdateTokens replaces the access token (and refresh when provided).
func (s *Accounts) UpdateTokens(ctx context.Context, accountID string, u TokenUpdate) error {
	tag, err := s.db.Exec(ctx, `
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
		INSERT INTO connected_accounts (tenant_id, provider, provider_account_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (provider, provider_account_id) DO UPDATE
			SET status = 'active', updated_at = now()
			WHERE connected_accounts.tenant_id = EXCLUDED.tenant_id
		RETURNING id::text`,
		c.TenantID, c.Provider, c.ProviderAccountID,
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
