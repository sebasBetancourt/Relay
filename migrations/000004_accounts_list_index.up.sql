-- GET /v1/accounts pages a tenant's accounts by (created_at, id). This index
-- serves that keyset scan and every tenant_id lookup, so the single-column
-- index it extends is dropped.
CREATE INDEX idx_connected_accounts_tenant_created ON connected_accounts (tenant_id, created_at, id);
DROP INDEX IF EXISTS idx_connected_accounts_tenant;
