# ADR 0005: Use golang.org/x/oauth2 for Microsoft, not MSAL

## Status
Accepted (2026-10-20)

## Context
The v0.1 scope in `docs/design.md` listed MSAL (Microsoft Authentication Library) for the Microsoft integration. By the time Microsoft is added, Relay already owns the whole token lifecycle for Google with `golang.org/x/oauth2`:
- state + PKCE in `oauth_states`;
- tokens encrypted per cell with AES-256-GCM + AAD in `oauth_tokens`;
- silent refresh with singleflight;
- revocation on `invalid_grant`.

Microsoft adds one hard requirement: it **rotates the refresh token on every refresh**. The newest one must be persisted before the new access token is used, and two API replicas must not race.

## Options considered
- **MSAL Go** (`microsoft-authentication-library-for-go`): the official library. It is built around its own token cache, where the library decides when to refresh and stores accounts and tokens through a cache interface. Plugging it into Relay would mean either:
  - storing MSAL's serialized cache blob, which is opaque, not per-cell encrypted with our AAD, and not a row we can lock; or
  - fighting the cache to keep our schema.

  The result would be two token lifecycles to maintain and audit: one for Google, one for Microsoft.
- **`golang.org/x/oauth2`** with `endpoints.AzureAD(tenant)`: the same library and code path Google already uses. Entra ID supports the standard authorization code flow with PKCE and refresh tokens, so nothing MSAL-specific is needed for delegated access to Graph.

## Decision
Use `golang.org/x/oauth2` for Microsoft. Each provider plugs in through `oauth.ProviderSpec`, which holds:
- the endpoint and scopes;
- its auth URL options;
- how to check the granted scopes;
- how to identify the account;
- how to revoke.

`Start`, `Finish`, refresh and revocation stay provider-neutral.

## Consequences
- **Good:**
  - One token lifecycle, one place to audit encryption, refresh-token rotation and revocation.
  - No extra dependency.
  - The fake-provider tests drive the same code for both providers.
- **Cost:** MSAL conveniences are out of scope and would have to be built if ever needed:
  - certificate credentials (client assertion);
  - managed identity;
  - token cache sharing with other apps.

  The client secret flow covers v0.1.
- **Revisit:** if Relay needs certificate-based client credentials, add them as a token-endpoint auth option, or reconsider MSAL for that path only.
