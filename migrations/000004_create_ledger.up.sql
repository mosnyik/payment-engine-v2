-- The double-entry ledger: the source of truth for every value movement in
-- the system. Every other module posts to this instead of being its own
-- source of truth — this is the direct structural fix for the v1 audit's
-- core finding (a mutable status column was the only record of "has this
-- been paid out", which a race condition exploited for a double payout).

-- NOTE: tenant_id has no FK to a `tenants` table yet — that table doesn't
-- exist until the tenant module (Phase 2) is built. Add the constraint via
-- a follow-up migration once it does; not adding a placeholder table here
-- to satisfy it early.
--
-- NULLS NOT DISTINCT (Postgres 15+) matters here: without it, two
-- platform-level accounts (tenant_id NULL) with the same account_type +
-- asset_code would NOT be treated as duplicates by a plain UNIQUE
-- constraint, since SQL normally treats every NULL as distinct from every
-- other NULL. That would silently defeat GetOrCreateAccount's upsert for
-- every omnibus/platform account.
CREATE TABLE ledger_accounts (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NULL,
    account_type  TEXT NOT NULL,
    asset_code    TEXT NOT NULL,
    unit_type     TEXT NOT NULL CHECK (unit_type IN ('crypto', 'fiat')),
    name          TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (tenant_id, account_type, asset_code)
);

CREATE TABLE ledger_transactions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key TEXT NOT NULL UNIQUE,
    txn_type        TEXT NOT NULL,
    reference_type  TEXT NOT NULL,
    reference_id    UUID NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ledger_transactions_reference ON ledger_transactions (reference_type, reference_id);

CREATE TABLE ledger_entries (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID NOT NULL REFERENCES ledger_transactions(id),
    account_id     UUID NOT NULL REFERENCES ledger_accounts(id),
    direction      TEXT NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount         NUMERIC(36, 18) NOT NULL CHECK (amount > 0),
    asset_code     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ledger_entries_transaction_id ON ledger_entries (transaction_id);
CREATE INDEX idx_ledger_entries_account_id ON ledger_entries (account_id);

-- Derived cache only — always reconstructable by summing ledger_entries
-- (debits positive, credits negative; see internal/ledger for the sign
-- convention). Never the source of truth; a reconciliation job comparing
-- this against a fresh sum is Phase 8 ops tooling, not built yet.
CREATE TABLE ledger_balances (
    account_id UUID PRIMARY KEY REFERENCES ledger_accounts(id),
    balance    NUMERIC(36, 18) NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
