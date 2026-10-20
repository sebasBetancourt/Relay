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

// ErrStateExpired is returned when Consume finds a row whose expires_at has
// passed. The row is still deleted: an expired state is useless.
var ErrStateExpired = errors.New("oauth state expired")

// OAuthState is one in-flight connect: created by GET /v1/connect/{provider},
// consumed once by the provider callback. CodeVerifier is the PKCE secret.
type OAuthState struct {
	State        string
	TenantID     string
	Provider     string
	CodeVerifier string
}

// ConsumedState is what the callback needs after a successful Consume.
type ConsumedState struct {
	TenantID     string
	CodeVerifier string
}

type OAuthStates struct {
	db *pgxpool.Pool
}

func NewOAuthStates(db *pgxpool.Pool) *OAuthStates {
	return &OAuthStates{db: db}
}

// Create stores a state that expires after ttl. expires_at uses the
// database clock (now()), the same clock the callback will compare against,
// so clock drift between API instances cannot shorten or extend the window.
// Unknown tenant → ErrNotFound.
func (s *OAuthStates) Create(ctx context.Context, st OAuthState, ttl time.Duration) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO oauth_states (state, tenant_id, provider, code_verifier, expires_at)
		VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`,
		st.State, st.TenantID, st.Provider, st.CodeVerifier, ttl.Seconds(),
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("create oauth state: %w", err)
	}
	return nil
}

// Consume deletes the state in one atomic statement and returns its secrets.
// A second call with the same state returns ErrNotFound (replay / double click).
// An expired row still disappears and returns ErrStateExpired.
//
// used_at is unused: the design says delete, not mark. Kept in schema for a
// future cleanup migration.
func (s *OAuthStates) Consume(ctx context.Context, state, provider string) (ConsumedState, error) {
	var (
		out     ConsumedState
		stillOK bool
	)
	err := s.db.QueryRow(ctx, `
		DELETE FROM oauth_states
		WHERE state = $1 AND provider = $2
		RETURNING tenant_id::text, code_verifier, expires_at > now()`,
		state, provider,
	).Scan(&out.TenantID, &out.CodeVerifier, &stillOK)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConsumedState{}, ErrNotFound
	}
	if err != nil {
		return ConsumedState{}, fmt.Errorf("consume oauth state: %w", err)
	}
	if !stillOK {
		return ConsumedState{}, ErrStateExpired
	}
	return out, nil
}

// DeleteExpired removes up to limit expired oauth_states rows.
// Uses FOR UPDATE SKIP LOCKED so concurrent janitors (or a Consume in
// flight) skip locked rows instead of blocking. Returns how many were deleted.
func (s *OAuthStates) DeleteExpired(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := s.db.Exec(ctx, `
		DELETE FROM oauth_states
		WHERE state IN (
			SELECT state FROM oauth_states
			WHERE expires_at < now()
			ORDER BY expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)`, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired oauth states: %w", err)
	}
	return tag.RowsAffected(), nil
}
