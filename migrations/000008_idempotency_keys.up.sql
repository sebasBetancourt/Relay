-- One row per Idempotency-Key. status NULL means the request is still running.
-- response is what the client got, so it holds event data: the janitor drops
-- rows after 24 hours, and they die with the tenant.
CREATE TABLE idempotency_keys (
    tenant_id    uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    key          text NOT NULL,
    request_hash text NOT NULL,
    status       integer,
    response     jsonb,
    locked_at    timestamptz NOT NULL DEFAULT now(),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);

CREATE INDEX idx_idempotency_keys_created ON idempotency_keys (created_at);
