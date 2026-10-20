package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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

type Accounts struct {
	db *pgxpool.Pool
}

func NewAccounts(db *pgxpool.Pool) *Accounts {
	return &Accounts{db: db}
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
