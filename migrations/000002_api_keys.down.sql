ALTER TABLE tenants
    ADD COLUMN api_key_hash text,
    ADD COLUMN key_prefix text;

-- Restore one key per tenant (the oldest active, else oldest).
UPDATE tenants t
SET api_key_hash = k.key_hash,
    key_prefix = k.key_prefix
FROM (
    SELECT DISTINCT ON (tenant_id) tenant_id, key_hash, key_prefix
    FROM api_keys
    ORDER BY tenant_id, revoked_at NULLS FIRST, created_at
) k
WHERE t.id = k.tenant_id;

ALTER TABLE tenants
    ALTER COLUMN api_key_hash SET NOT NULL,
    ALTER COLUMN key_prefix SET NOT NULL,
    ADD CONSTRAINT tenants_api_key_hash_key UNIQUE (api_key_hash);

DROP TABLE IF EXISTS api_keys;

ALTER TABLE tenants
    DROP CONSTRAINT IF EXISTS tenants_name_len;
