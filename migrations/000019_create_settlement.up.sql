-- Phase 6's settlement module (ARCHITECTURE.md §8): the ledger-claim-then-
-- dispatch pipeline fired by session.deposit_confirmed. Three tables:
-- settlements (one row per session's settlement lifecycle), settlement_attempts
-- (one row per dispatch attempt, across providers/retries), settlement_reversals
-- (the ops queue for a post-settled provider/bank return).
CREATE TABLE settlements (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id               UUID NOT NULL UNIQUE REFERENCES sessions(id),
    tenant_id                UUID NOT NULL REFERENCES tenants(id),
    corridor_id              UUID NOT NULL REFERENCES corridors(id),
    status                   TEXT NOT NULL DEFAULT 'pending_dispatch' CHECK (status IN (
        'pending_dispatch','dispatching','settling','settled',
        'settlement_failed','reversed','reversal_resolved')),
    -- crypto_asset/fiat_currency are known (from the session) at insert
    -- time; the amount columns default to 0 until DispatchWorker computes
    -- them from the actual confirmed deposit + rate lock + fee (dispatch.go)
    -- — kept NOT NULL throughout rather than nullable so every reader
    -- (ListSettlements included) can scan a freshly-inserted pending_dispatch
    -- row without special-casing NULL.
    crypto_asset             TEXT NOT NULL,
    crypto_amount            NUMERIC(36,18) NOT NULL DEFAULT 0,
    fiat_currency            TEXT NOT NULL,
    fiat_value               NUMERIC(36,18) NOT NULL DEFAULT 0,
    fee_amount               NUMERIC(36,18) NOT NULL DEFAULT 0,
    tenant_payable_amount    NUMERIC(36,18) NOT NULL DEFAULT 0,
    attempt_count            INT NOT NULL DEFAULT 0,
    confirmation_deadline_at TIMESTAMPTZ,
    ops_paged_at             TIMESTAMPTZ,
    -- Set by an ops-triggered retry (RetryPayout) supplying corrected bank
    -- details; consumed and cleared by the next dispatch attempt. NULL
    -- means "use whatever the previous attempt used" (opaque either way —
    -- no tenant-level payout-destination schema exists yet).
    pending_destination      JSONB,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The dispatch worker's claim query.
CREATE INDEX idx_settlements_status ON settlements (status);

-- The bucket-3 confirmation-timeout job's sweep query: only a dispatched,
-- not-yet-paged settlement still waiting on a provider webhook.
CREATE INDEX idx_settlements_confirmation_deadline ON settlements (confirmation_deadline_at)
    WHERE status = 'settling' AND ops_paged_at IS NULL;

CREATE TABLE settlement_attempts (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    settlement_id      UUID NOT NULL REFERENCES settlements(id),
    attempt_number     INT NOT NULL,
    provider_name      TEXT NOT NULL,
    -- 'settlement_payout:session_X:attempt_N' — the ledger's atomic
    -- pre-dispatch claim key (ARCHITECTURE.md §6).
    idempotency_key    TEXT NOT NULL UNIQUE,
    status             TEXT NOT NULL DEFAULT 'dispatched' CHECK (status IN (
        'dispatched','succeeded','failed_bucket1','failed_bucket2','failed_bucket4')),
    provider_reference TEXT,
    failure_reason     TEXT,
    -- Opaque payout-destination payload (bank details) supplied at dispatch/
    -- retry time — no tenant-level payout-destination schema exists yet.
    provider_payload   JSONB NOT NULL DEFAULT '{}',
    dispatched_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at        TIMESTAMPTZ,
    UNIQUE (settlement_id, attempt_number)
);

-- Webhook resolution: find the attempt a provider callback refers to.
CREATE INDEX idx_settlement_attempts_provider_ref ON settlement_attempts (provider_name, provider_reference)
    WHERE provider_reference IS NOT NULL;

CREATE TABLE settlement_reversals (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    settlement_id   UUID NOT NULL REFERENCES settlements(id),
    session_id      UUID NOT NULL REFERENCES sessions(id),
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved')),
    reason          TEXT,
    reported_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_by     UUID,
    resolved_at     TIMESTAMPTZ,
    resolution_note TEXT
);

-- The ops queue view.
CREATE INDEX idx_settlement_reversals_open ON settlement_reversals (status)
    WHERE status = 'open';
