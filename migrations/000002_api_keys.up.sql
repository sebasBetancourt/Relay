-- Enforce name length in the DB (defense in depth vs app-only checks).
ALTER TABLE tenants
    ADD CONSTRAINT tenants_name_len CHECK (char_length(name) BETWEEN 1 AND 200);

-- One tenant, many keys. Hash lives here so keys can be rotated/revoked independently.
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    key_hash     text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at   timestamptz
);

CREATE INDEX idx_api_keys_tenant ON api_keys (tenant_id);
CREATE INDEX idx_api_keys_active_hash ON api_keys (key_hash) WHERE revoked_at IS NULL;

-- Move keys off tenants (v0.1 rows, if any).
INSERT INTO api_keys (tenant_id, key_hash, key_prefix, created_at)
SELECT id, api_key_hash, key_prefix, created_at
FROM tenants;

ALTER TABLE tenants
    DROP COLUMN api_key_hash,
    DROP COLUMN key_prefix;
