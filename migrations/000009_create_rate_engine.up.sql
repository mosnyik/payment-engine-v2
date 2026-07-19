-- Rate engine (see ARCHITECTURE.md §7). Ported from v1 with three
-- adaptations: Postgres instead of MySQL, a corridor-driven fiat-currency
-- list instead of a hardcoded one, and a persisted rate_locks table
-- instead of an in-memory-only lock.

-- The ops-configured ceiling rate per fiat currency — external providers
-- can only push the locked rate down from here, never up. Keyed by
-- fiat_currency (not a single global row like v1) since the active
-- currency list is corridor-driven, not hardcoded to NGN.
CREATE TABLE system_rates (
    fiat_currency  TEXT PRIMARY KEY,
    current_rate   NUMERIC(20, 6) NOT NULL,
    merchant_rate  NUMERIC(20, 6) NOT NULL DEFAULT 0,
    profit_rate    NUMERIC(20, 6) NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The last rate fetched from each external provider. Written by the
-- background fetch job on a schedule; read by the aggregator during
-- LockRate so no external API call ever sits on the transaction path.
CREATE TABLE provider_rates (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider      TEXT NOT NULL,
    fiat_currency TEXT NOT NULL,
    rate          NUMERIC(20, 6) NOT NULL,
    fetched_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, fiat_currency)
);

-- A concrete, audit-able record of every locked quote — v1 kept this only
-- in memory, embedded into the session. Gives the ledger's fx_conversion
-- entries a durable record of which quote and provider produced the rate.
CREATE TABLE rate_locks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    crypto_asset    TEXT NOT NULL,
    fiat_currency   TEXT NOT NULL,
    rate            NUMERIC(20, 6) NOT NULL,
    asset_price_usd NUMERIC(20, 8) NOT NULL,
    provider        TEXT NOT NULL,
    locked_at       TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
