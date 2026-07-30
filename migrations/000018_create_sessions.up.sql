-- Phase 5's session state machine (ARCHITECTURE.md §8). Full 11-state enum
-- included now even though settling/settled/settlement_failed/reversed/
-- reversal_resolved aren't driven until Phase 6 (settlement) exists — the
-- state machine itself is already fully designed, and adding states later
-- would mean another migration touching a CHECK constraint on a live table.
CREATE TABLE sessions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL REFERENCES tenants(id),
    corridor_id             UUID NOT NULL REFERENCES corridors(id),
    status                  TEXT NOT NULL DEFAULT 'screening' CHECK (status IN (
        'screening','compliance_hold','rejected','pending','expired',
        'deposit_detected','deposit_confirmed','settling','settled',
        'settlement_failed','reversed','reversal_resolved')),
    fiat_currency           TEXT NOT NULL,
    fiat_amount             NUMERIC(36,18) NOT NULL CHECK (fiat_amount > 0),
    crypto_asset            TEXT NOT NULL,
    crypto_network          TEXT NOT NULL,
    compliance_case_id      UUID NULL REFERENCES compliance_cases(id),
    rate_lock_id            UUID NULL REFERENCES rate_locks(id),
    deposit_reservation_id  UUID NULL REFERENCES treasury_address_reservations(id),
    sla_breached_at         TIMESTAMPTZ NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sessions_tenant ON sessions (tenant_id);

-- The TTL/SLA background job's sweep query: only sessions that could still
-- be pre-deposit need to be scanned every tick.
CREATE INDEX idx_sessions_ttl_sweep ON sessions (created_at)
    WHERE status IN ('screening','compliance_hold','pending');

-- Deposit-side event handlers (treasury.deposit_detected/confirmed) look up
-- the owning session by reservation id.
CREATE INDEX idx_sessions_deposit_reservation ON sessions (deposit_reservation_id)
    WHERE deposit_reservation_id IS NOT NULL;
