package oauth

import (
	"context"
)

// Disconnect removes a tenant's account (DELETE /v1/accounts/{id}).
// Relay's database is the source of truth: the tokens are deleted and the
// account marked disconnected first, then the token is revoked at the
// provider, best effort. A provider outage never blocks a disconnect, and
// Relay never keeps tokens it was asked to forget. Idempotent.
// Missing or wrong-tenant account → store.ErrNotFound.
func (s *Service) Disconnect(ctx context.Context, tenantID, accountID string) error {
	prev, err := s.accounts.Disconnect(ctx, accountID, tenantID)
	if err != nil {
		return err
	}
	if prev.Status == "disconnected" {
		return nil // already done; tokens were deleted the first time
	}
	s.log.Info("account disconnected", "account_id", prev.ID, "provider", prev.Provider)

	spec, ok := s.providers[Provider(prev.Provider)]
	if !ok || spec.Revoke == nil || !prev.HasTokens {
		return nil
	}
	// Revoking the refresh token also kills its access tokens (Google).
	column, enc, nonce := "refresh_token", prev.RefreshEnc, prev.RefreshNonce
	if len(enc) == 0 {
		column, enc, nonce = "access_token", prev.AccessEnc, prev.AccessNonce
	}
	token, err := s.decrypt(prev, column, enc, nonce)
	if err != nil {
		return nil // logged by decrypt; the local disconnect already succeeded
	}
	// The client may hang up after the 204 is decided; finish the revoke
	// anyway (the HTTP client's timeout still bounds it).
	if err := spec.Revoke(context.WithoutCancel(ctx), s.http, token); err != nil {
		s.log.Warn("provider revoke failed", "account_id", prev.ID, "provider", prev.Provider, "err", err)
		return nil
	}
	s.log.Info("provider token revoked", "account_id", prev.ID, "provider", prev.Provider)
	return nil
}
