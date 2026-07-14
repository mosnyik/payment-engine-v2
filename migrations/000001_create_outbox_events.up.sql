CREATE TABLE outbox_events (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type     TEXT NOT NULL,
    aggregate_type TEXT NOT NULL,
    aggregate_id   UUID NOT NULL,
    payload        JSONB NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at  TIMESTAMPTZ NULL,
    attempts       INT NOT NULL DEFAULT 0
);

-- Dispatcher scans undispatched rows in creation order; partial index keeps
-- the scan cheap as dispatched rows accumulate.
CREATE INDEX idx_outbox_events_undispatched ON outbox_events (created_at)
    WHERE dispatched_at IS NULL;
