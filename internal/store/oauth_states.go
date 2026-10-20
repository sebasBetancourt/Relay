package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OAuthState is one in-flight connect: created by GET /v1/connect/{provider},
// consumed once by the provider callback. CodeVerifier is the PKCE secret.
type OAuthState struct {
	State        string
	TenantID     string
	Provider     string
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
