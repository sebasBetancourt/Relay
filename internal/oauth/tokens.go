package oauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/sebasBetancourt/relay/internal/store"
	"github.com/sebasBetancourt/relay/internal/vault"
)

// refreshSkew: refresh when the token dies within this window so we do not
// hand a near-dead token to the provider API mid-request.
const refreshSkew = 60 * time.Second

var (
	// ErrAccountRevoked: the account is not active, or the provider no
	// longer accepts its refresh token. The user must connect again.
	ErrAccountRevoked = errors.New("account revoked")

	// errNeedsRevoke is returned from inside the refresh lock: revoking
	// writes the same account, so it must happen after the lock is released.
	errNeedsRevoke = errors.New("refresh token no longer valid")
)

// AccessToken is a usable bearer token for a provider API call.
type AccessToken struct {
	Value    string
	Provider Provider
}

// tokenAAD binds ciphertext to table + tenant + provider account + column.
// Must stay identical for Encrypt and Decrypt of the same cell.
func tokenAAD(tenantID, provider, sub, column string) []byte {
	return vault.AAD("oauth_tokens", tenantID, provider, sub, column)
}

// AccessToken returns a valid access token for the account, refreshing when
// expires_at is within refreshSkew. Missing / wrong-tenant → store.ErrNotFound.
func (s *Service) AccessToken(ctx context.Context, tenantID, accountID string) (AccessToken, error) {
	return s.token(ctx, tenantID, accountID, false)
}

// RefreshAccessToken forces a refresh (e.g. after a provider 401).
func (s *Service) RefreshAccessToken(ctx context.Context, tenantID, accountID string) (AccessToken, error) {
	return s.token(ctx, tenantID, accountID, true)
}

func (s *Service) token(ctx context.Context, tenantID, accountID string, force bool) (AccessToken, error) {
	acct, err := s.accounts.AccountForTenant(ctx, accountID, tenantID)
	if err != nil {
		return AccessToken{}, err
	}
	if acct.Status != "active" {
		return AccessToken{}, ErrAccountRevoked
	}
	if !acct.HasTokens || len(acct.AccessEnc) == 0 {
		return AccessToken{}, fmt.Errorf("access token: active account without tokens")
	}

	access, err := s.decrypt(acct, "access_token", acct.AccessEnc, acct.AccessNonce)
	if err != nil {
		return AccessToken{}, err
	}
	if !force && time.Until(acct.ExpiresAt) > refreshSkew {
		return AccessToken{Value: access, Provider: Provider(acct.Provider)}, nil
	}
	return s.refresh(ctx, acct)
}

// refresh coordinates at two levels: singleflight collapses concurrent
// callers inside this process, and store.RefreshLocked serializes replicas
// through a row lock in Postgres.
func (s *Service) refresh(ctx context.Context, seen store.StoredAccount) (AccessToken, error) {
	v, err, _ := s.flight.Do(seen.ID, func() (any, error) {
		return s.doRefresh(ctx, seen)
	})
	if err != nil {
		return AccessToken{}, err
	}
	return v.(AccessToken), nil
}

// doRefresh runs the provider refresh under the account's row lock.
// seen is the row this request read before deciding to refresh.
func (s *Service) doRefresh(ctx context.Context, seen store.StoredAccount) (AccessToken, error) {
	var (
		out           AccessToken
		refreshed     bool
		revokeReason  string
		provider      = Provider(seen.Provider)
		ctxWithClient = context.WithValue(ctx, oauth2.HTTPClient, s.http)
	)

	err := s.accounts.RefreshLocked(ctx, seen.ID, func(cur store.StoredAccount) (*store.TokenUpdate, error) {
		// Another replica (or request) stored a new token while we waited
		// for the lock: use it instead of refreshing again. Comparing the
		// nonce tells "new ciphertext" apart from "the token we already saw".
		if !bytes.Equal(cur.AccessNonce, seen.AccessNonce) && time.Until(cur.ExpiresAt) > refreshSkew {
			access, err := s.decrypt(cur, "access_token", cur.AccessEnc, cur.AccessNonce)
			if err != nil {
				return nil, err
			}
			out = AccessToken{Value: access, Provider: provider}
			return nil, nil
		}

		if len(cur.RefreshEnc) == 0 {
			revokeReason = "no_refresh_token"
			return nil, errNeedsRevoke
		}
		refreshPlain, err := s.decrypt(cur, "refresh_token", cur.RefreshEnc, cur.RefreshNonce)
		if err != nil {
			return nil, err
		}

		spec, ok := s.providers[provider]
		if !ok {
			return nil, fmt.Errorf("refresh: %w", ErrUnsupportedProvider)
		}
		tok, err := spec.OAuth.TokenSource(ctxWithClient, &oauth2.Token{
			RefreshToken: refreshPlain,
			Expiry:       time.Now().Add(-time.Minute),
		}).Token()
		if err != nil {
			if reason, revoke := revokeOnRefreshError(err); revoke {
				revokeReason = reason
				return nil, errNeedsRevoke
			}
			return nil, mapRefreshError(err)
		}

		update, err := s.encryptRefreshed(cur, tok, refreshPlain)
		if err != nil {
			return nil, err
		}
		out = AccessToken{Value: tok.AccessToken, Provider: provider}
		refreshed = true
		// Written in the same transaction as the lock: a rotated refresh
		// token is stored before anyone can use the new access token.
		return update, nil
	})

	switch {
	case errors.Is(err, errNeedsRevoke):
		if markErr := s.accounts.MarkRevoked(ctx, seen.ID); markErr != nil && !errors.Is(markErr, store.ErrNotFound) {
			return AccessToken{}, fmt.Errorf("refresh: mark revoked: %w", markErr)
		}
		s.log.Warn("account revoked", "account_id", seen.ID, "provider", seen.Provider, "reason", revokeReason)
		return AccessToken{}, ErrAccountRevoked
	case errors.Is(err, store.ErrNotFound):
		// Revoked or disconnected by someone else while we waited.
		return AccessToken{}, ErrAccountRevoked
	case err != nil:
		// Includes a failed commit after the provider already rotated the
		// refresh token. The stored (previous) refresh token is left in
		// place: Microsoft keeps it valid until it expires, so the next
		// request retries; if the provider rejects it, invalid_grant
		// revokes the account then.
		return AccessToken{}, err
	}

	if refreshed {
		s.log.Info("token refreshed", "account_id", seen.ID, "provider", seen.Provider)
	}
	return out, nil
}

// encryptRefreshed seals the new access token and, if the provider rotated
// it (Microsoft does on every refresh), the new refresh token.
func (s *Service) encryptRefreshed(cur store.StoredAccount, tok *oauth2.Token, oldRefresh string) (*store.TokenUpdate, error) {
	accessEnc, accessNonce, err := s.vault.Encrypt(
		[]byte(tok.AccessToken),
		tokenAAD(cur.TenantID, cur.Provider, cur.ProviderAccountID, "access_token"),
	)
	if err != nil {
		return nil, fmt.Errorf("refresh: encrypt access: %w", err)
	}
	update := &store.TokenUpdate{AccessEnc: accessEnc, AccessNonce: accessNonce, ExpiresAt: tok.Expiry}

	if tok.RefreshToken != "" && tok.RefreshToken != oldRefresh {
		update.RefreshEnc, update.RefreshNonce, err = s.vault.Encrypt(
			[]byte(tok.RefreshToken),
			tokenAAD(cur.TenantID, cur.Provider, cur.ProviderAccountID, "refresh_token"),
		)
		if err != nil {
			return nil, fmt.Errorf("refresh: encrypt refresh: %w", err)
		}
	}
	return update, nil
}

func (s *Service) decrypt(acct store.StoredAccount, column string, enc, nonce []byte) (string, error) {
	plain, err := s.vault.Decrypt(enc, nonce, tokenAAD(acct.TenantID, acct.Provider, acct.ProviderAccountID, column))
	if err != nil {
		// Corrupt row or wrong key: an operator problem, never a client one.
		s.log.Error("token decrypt failed",
			"account_id", acct.ID,
			"provider", acct.Provider,
			"column", column,
			"err", err,
		)
		return "", fmt.Errorf("decrypt %s: %w", column, err)
	}
	return string(plain), nil
}

// revokeOnRefreshError reports whether the provider says the refresh token
// is dead for good, which only a new connect can fix:
//   - invalid_grant: revoked, expired, password changed, app removed.
//   - interaction_required: Microsoft Entra ID needs the user again
//     (MFA, Conditional Access, new consent).
func revokeOnRefreshError(err error) (reason string, revoke bool) {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return "", false
	}
	for _, code := range []string{"invalid_grant", "interaction_required"} {
		if strings.EqualFold(re.ErrorCode, code) || strings.Contains(string(re.Body), `"`+code+`"`) {
			return code, true
		}
	}
	return "", false
}

func mapRefreshError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.Response != nil && re.Response.StatusCode >= 500 {
			return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
		}
		// Other 4xx (e.g. invalid_client): our config is wrong, internal.
		return fmt.Errorf("refresh: provider 4xx: %w", err)
	}
	return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
}
