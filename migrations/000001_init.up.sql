CREATE TYPE provider AS ENUM ('google', 'microsoft');
CREATE TYPE account_status AS ENUM ('active', 'revoked', 'disconnected', 'error');

CREATE TABLE tenants (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text NOT NULL,
    api_key_hash text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE connected_accounts (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    provider            provider NOT NULL,
    provider_account_id text NOT NULL,
    status              account_status NOT NULL DEFAULT 'active',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_account_id)
);

CREATE TABLE oauth_tokens (
    account_id         uuid PRIMARY KEY REFERENCES connected_accounts (id) ON DELETE CASCADE,
    access_token_enc   bytea NOT NULL,
    refresh_token_enc  bytea,
    access_nonce       bytea NOT NULL,
    refresh_nonce      bytea,
    expires_at         timestamptz NOT NULL,
    scopes             text[] NOT NULL DEFAULT '{}',
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE oauth_states (
    state         text PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    provider      provider NOT NULL,
    code_verifier text NOT NULL,
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz
);

CREATE INDEX idx_connected_accounts_tenant ON connected_accounts (tenant_id);
CREATE INDEX idx_oauth_states_expires ON oauth_states (expires_at);
