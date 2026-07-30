-- Self-custody HD wallets (Phase 4 step 2, see ARCHITECTURE.md §2/§3).
-- Ported from v1's HD wallet/watcher/sweeper (real, working code there,
-- unlike Busha) with two deliberate fixes for audit lessons 2 and 3.

-- Singleton row holding the encrypted BIP39 mnemonic. Fixes audit lesson 2
-- (v1 kept the ciphertext and its decryption key in the same .env file):
-- the ciphertext lives here in the DB, the key lives in config
-- (HD_WALLET_SEED_ENCRYPTION_KEY) — structurally separated, populated once
-- via `adminctl -init-wallet`.
CREATE TABLE hd_wallet_seed (
    id                 SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    mnemonic_ciphertext TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Next unused derivation index per chain, for allocating a brand-new HD
-- address once no free (unreserved) derived_addresses row is available.
-- Allocation is `SELECT ... FOR UPDATE` on the chain's row — same locking
-- convention already used elsewhere (e.g. hd_wallet-style index allocation
-- in v1, and ledger/corridor's own row-locking patterns).
CREATE TABLE hd_wallet_indices (
    chain      TEXT PRIMARY KEY,
    next_index BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per HD-derived (chain, derivation_index) -> address. An address
-- is derived once and reused across many reservations over time
-- (ARCHITECTURE.md §3: "Reuse per end-user").
CREATE TABLE derived_addresses (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    chain            TEXT NOT NULL,
    derivation_index BIGINT NOT NULL,
    address          TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (chain, derivation_index),
    UNIQUE (address)
);

-- Fixes audit lesson 3 (v1: app-level check-then-insert let one deposit
-- fund two sessions via a reused address, with only a plain index behind
-- it). A second open reservation on the same address is now a constraint
-- violation, not a race an app-level check can lose.
CREATE UNIQUE INDEX idx_treasury_reservations_open_address
    ON treasury_address_reservations (address)
    WHERE status = 'reserved';

-- Ops-configured destination address sweeps are sent to, per chain/asset —
-- config-driven like corridor bindings, not hardcoded or env-based, since
-- this is the kind of thing ops change without a redeploy.
CREATE TABLE sweep_destinations (
    chain               TEXT NOT NULL,
    crypto_asset        TEXT NOT NULL,
    destination_address TEXT NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (chain, crypto_asset)
);

-- The system's own outbound-transaction audit trail — separate from
-- treasury_deposits (inbound). One row per attempted sweep.
CREATE TABLE treasury_sweeps (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reservation_id      UUID NOT NULL REFERENCES treasury_address_reservations(id),
    crypto_asset        TEXT NOT NULL,
    amount              NUMERIC(20, 8) NOT NULL,
    destination_address TEXT NOT NULL,
    tx_hash             TEXT,
    status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'broadcast', 'confirmed', 'failed')),
    error               TEXT,
    attempted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
