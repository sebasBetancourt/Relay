-- Incremental calendar sync. cursor is a Google syncToken or a Graph
-- deltaLink: a provider secret, never logged. Rows die with the account
-- (ON DELETE CASCADE). Disconnect also deletes them while the account row
-- stays, because disconnect does not remove connected_accounts.
CREATE TABLE account_sync_state (
    account_id     uuid PRIMARY KEY REFERENCES connected_accounts (id) ON DELETE CASCADE,
    cursor         text NOT NULL DEFAULT '',
    window_start   timestamptz,
    window_end     timestamptz,
    full_synced_at timestamptz,
    last_synced_at timestamptz,
    status         text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ok', 'error')),
    last_error     text NOT NULL DEFAULT ''
);

CREATE INDEX idx_account_sync_state_last_synced ON account_sync_state (last_synced_at);

-- One normalized copy of each provider event. deleted is a tombstone:
-- a later sync must still see the id so a replay stays idempotent.
CREATE TABLE synced_events (
    account_id        uuid NOT NULL REFERENCES connected_accounts (id) ON DELETE CASCADE,
    provider_event_id text NOT NULL,
    version           text NOT NULL DEFAULT '',
    data              jsonb NOT NULL,
    deleted           boolean NOT NULL DEFAULT false,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, provider_event_id)
);
