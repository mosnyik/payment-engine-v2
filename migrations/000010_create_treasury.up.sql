-- Treasury (see ARCHITECTURE.md §2/§3, module map §5). Phase 4 step 1 only:
-- the Busha partner-custodied collection adapter. Self-custody HD wallets
-- (key derivation, chain watchers, sweep policy) are a separate future
-- step — no sweep-execution table here, sweep is a self-custody concern.

-- One reservation per address handed to a depositor. custody_type records
-- whether the bound provider is self-custody or partner-custodied (a
-- property of the provider, not fixed system-wide — ARCHITECTURE.md §2).
CREATE TABLE treasury_address_reservations (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    corridor_id        UUID NOT NULL REFERENCES corridors(id),
    provider_name      TEXT NOT NULL,
    custody_type       TEXT NOT NULL CHECK (custody_type IN ('self_custody', 'partner_custodied')),
    crypto_asset       TEXT NOT NULL,
    crypto_network     TEXT NOT NULL,
    address            TEXT NOT NULL,
    address_tag        TEXT,
    provider_reference TEXT,
    status             TEXT NOT NULL DEFAULT 'reserved' CHECK (status IN ('reserved', 'released')),
    reserved_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Webhook-driven deposit state for one reservation. UNIQUE(reservation_id,
-- tx_reference) makes a replayed Busha webhook a no-op (ON CONFLICT DO
-- NOTHING) rather than double-processing the same deposit.
CREATE TABLE treasury_deposits (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reservation_id   UUID NOT NULL REFERENCES treasury_address_reservations(id),
    status           TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'detected', 'confirmed')),
    crypto_asset     TEXT NOT NULL,
    amount           NUMERIC(20, 8) NOT NULL,
    tx_reference     TEXT NOT NULL,
    provider_payload JSONB NOT NULL DEFAULT '{}',
    detected_at      TIMESTAMPTZ,
    confirmed_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (reservation_id, tx_reference)
);

-- Latest known balance a partner-custodied provider reports holding on our
-- behalf, per asset — a snapshot for reconciliation, not an append-only
-- ledger (that's internal/ledger's job once settlement posts against it).
CREATE TABLE treasury_custody_balances (
    provider_name TEXT NOT NULL,
    crypto_asset  TEXT NOT NULL,
    balance       NUMERIC(20, 8) NOT NULL,
    as_of         TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_name, crypto_asset)
);
