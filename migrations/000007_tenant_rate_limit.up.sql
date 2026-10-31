-- Per-tenant override of RATE_LIMIT_PER_MINUTE. NULL means the global default.
ALTER TABLE tenants
    ADD COLUMN rate_limit_per_minute integer
        CONSTRAINT tenants_rate_limit_positive CHECK (rate_limit_per_minute > 0);
