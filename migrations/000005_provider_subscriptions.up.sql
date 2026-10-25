-- Push subscriptions: one Google watch channel or Graph subscription per account.
-- The plaintext channel token / clientState is never stored, only its hash.
CREATE TABLE provider_subscriptions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  uuid NOT NULL UNIQUE REFERENCES connected_accounts (id) ON DELETE CASCADE,
    provider    provider NOT NULL,
    external_id text NOT NULL,
    resource_id text NOT NULL DEFAULT '',
    secret_hash text NOT NULL,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, external_id)
);

CREATE INDEX idx_provider_subscriptions_expires ON provider_subscriptions (expires_at);
