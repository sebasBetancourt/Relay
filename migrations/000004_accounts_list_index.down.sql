CREATE INDEX IF NOT EXISTS idx_connected_accounts_tenant ON connected_accounts (tenant_id);
DROP INDEX IF EXISTS idx_connected_accounts_tenant_created;
