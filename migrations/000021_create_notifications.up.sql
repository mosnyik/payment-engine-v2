-- Phase 7's notification module (ARCHITECTURE.md's module map: "Webhook
-- subscriber config, delivery log, dead-letter queue"). One shared delivery
-- log for both channels (webhook, email) — retry/dead-letter state is
-- identical regardless of channel; only destination and the send mechanism
-- differ, so a stub channel (email) doesn't need its own copy of the whole
-- retry state machine.
CREATE TABLE notification_deliveries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    event_type      TEXT NOT NULL,
    aggregate_type  TEXT NOT NULL,
    aggregate_id    UUID NOT NULL,
    channel         TEXT NOT NULL CHECK (channel IN ('webhook', 'email')),
    -- Webhook URL or email address, snapshotted at creation time — a later
    -- change to the tenant's webhook_url must not retroactively alter a
    -- delivery already in flight.
    destination     TEXT NOT NULL,
    -- The outbound body: event_type/aggregate_type/aggregate_id/tenant_id/
    -- payload/created_at.
    payload         JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending_dispatch' CHECK (status IN (
        'pending_dispatch', 'delivered', 'dead_letter')),
    attempt_count   INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    delivered_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Defensive belt-and-suspenders against a redelivered underlying event
    -- creating a duplicate row — same caution as settlement's
    -- ON CONFLICT DO NOTHING precedent (internal/settlement/events.go).
    UNIQUE (aggregate_id, event_type, channel)
);

-- DispatchWorker's claim query: only rows actually due.
CREATE INDEX idx_notification_deliveries_dispatch
    ON notification_deliveries (next_attempt_at)
    WHERE status = 'pending_dispatch';

-- The ops dead-letter queue view.
CREATE INDEX idx_notification_deliveries_status ON notification_deliveries (status);
