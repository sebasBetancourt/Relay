package oauth

import (
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
// hand a near-dead token to the Calendar API mid-request.
const refreshSkew = 60 * time.Second

var (
	// ErrAccountRevoked: account is not active, or Google rejected the refresh.
	ErrAccountRevoked = errors.New("account revoked")
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

	provider := Provider(acct.Provider)
	access, err := s.vault.Decrypt(
		acct.AccessEnc, acct.AccessNonce,
		tokenAAD(acct.TenantID, acct.Provider, acct.ProviderAccountID, "access_token"),
	)
	if err != nil {
		s.log.Error("token decrypt failed",
			"account_id", accountID,
			"provider", acct.Provider,
			"err", err,
		)
		return AccessToken{}, fmt.Errorf("access token: decrypt: %w", err)
	}

	if !force && time.Until(acct.ExpiresAt) > refreshSkew {
		return AccessToken{Value: string(access), Provider: provider}, nil
	}

	return s.refresh(ctx, acct)
}

func (s *Service) refresh(ctx context.Context, acct store.StoredAccount) (AccessToken, error) {
	v, err, _ := s.flight.Do(acct.ID, func() (any, error) {
		return s.doRefresh(ctx, acct)
	})
	if err != nil {
		return AccessToken{}, err
	}
	return v.(AccessToken), nil
}

func (s *Service) doRefresh(ctx context.Context, acct store.StoredAccount) (AccessToken, error) {
	if len(acct.RefreshEnc) == 0 {
		if err := s.accounts.MarkRevoked(ctx, acct.ID); err != nil {
			return AccessToken{}, fmt.Errorf("refresh: mark revoked: %w", err)
		}
		s.log.Warn("account revoked", "account_id", acct.ID, "provider", acct.Provider, "reason", "no_refresh_token")
		return AccessToken{}, ErrAccountRevoked
	}

	refreshPlain, err := s.vault.Decrypt(
		acct.RefreshEnc, acct.RefreshNonce,
		tokenAAD(acct.TenantID, acct.Provider, acct.ProviderAccountID, "refresh_token"),
	)
	if err != nil {
		s.log.Error("refresh decrypt failed",
			"account_id", acct.ID,
			"provider", acct.Provider,
			"err", err,
		)
		return AccessToken{}, fmt.Errorf("refresh: decrypt: %w", err)
	}

	provider := Provider(acct.Provider)
	spec, ok := s.providers[provider]
	if !ok {
		return AccessToken{}, fmt.Errorf("refresh: %w", ErrUnsupportedProvider)
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, s.http)
	ts := spec.OAuth.TokenSource(ctx, &oauth2.Token{
		RefreshToken: string(refreshPlain),
		Expiry:       time.Now().Add(-time.Minute),
	})
	tok, err := ts.Token()
	if err != nil {
		return AccessToken{}, s.mapRefreshError(ctx, acct, err)
	}

	accessEnc, accessNonce, err := s.vault.Encrypt(
		[]byte(tok.AccessToken),
		tokenAAD(acct.TenantID, acct.Provider, acct.ProviderAccountID, "access_token"),
	)
	if err != nil {
		return AccessToken{}, fmt.Errorf("refresh: encrypt access: %w", err)
	}

	var refreshEnc, refreshNonce []byte
	if tok.RefreshToken != "" && tok.RefreshToken != string(refreshPlain) {
		refreshEnc, refreshNonce, err = s.vault.Encrypt(
			[]byte(tok.RefreshToken),
			tokenAAD(acct.TenantID, acct.Provider, acct.ProviderAccountID, "refresh_token"),
		)
		if err != nil {
			return AccessToken{}, fmt.Errorf("refresh: encrypt refresh: %w", err)
		}
	}

	if err := s.accounts.UpdateTokens(ctx, acct.ID, store.TokenUpdate{
		AccessEnc:    accessEnc,
		AccessNonce:  accessNonce,
		RefreshEnc:   refreshEnc,
		RefreshNonce: refreshNonce,
		ExpiresAt:    tok.Expiry,
	}); err != nil {
		return AccessToken{}, err
	}

	s.log.Info("token refreshed", "account_id", acct.ID, "provider", acct.Provider)
	return AccessToken{Value: tok.AccessToken, Provider: provider}, nil
}

func (s *Service) mapRefreshError(ctx context.Context, acct store.StoredAccount, err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if isInvalidGrant(re) {
			if markErr := s.accounts.MarkRevoked(ctx, acct.ID); markErr != nil {
				return fmt.Errorf("refresh: mark revoked: %w", markErr)
			}
			s.log.Warn("account revoked", "account_id", acct.ID, "provider", acct.Provider, "reason", "invalid_grant")
			return ErrAccountRevoked
		}
		if re.Response != nil && re.Response.StatusCode >= 500 {
			return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
		}
		// Other 4xx (e.g. invalid_client): our config is wrong — internal.
		return fmt.Errorf("refresh: provider 4xx: %w", err)
	}
	return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
}

func isInvalidGrant(re *oauth2.RetrieveError) bool {
	if strings.EqualFold(re.ErrorCode, "invalid_grant") {
		return true
	}
	return strings.Contains(string(re.Body), "invalid_grant")
}
