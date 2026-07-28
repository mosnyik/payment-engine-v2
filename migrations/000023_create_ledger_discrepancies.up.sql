-- Phase 8's reconciliation job (internal/ledger/reconcile.go). One row per
-- detected drift occurrence, not upserted/deduped across runs — same
-- historical-row-set convention notification_deliveries/compliance_cases
-- already use rather than a single mutable "current state" row. The job
-- never writes ledger_balances itself (see that table's own comment in
-- 000004_create_ledger.up.sql — "never the source of truth"); this table is
-- purely a flag for ops to review and close manually.
CREATE TABLE ledger_discrepancies (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id       UUID NOT NULL REFERENCES ledger_accounts(id),
    cached_balance   NUMERIC(36, 18) NOT NULL,
    computed_balance NUMERIC(36, 18) NOT NULL,
    drift_amount     NUMERIC(36, 18) NOT NULL,
    detected_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at      TIMESTAMPTZ,
    resolved_by      UUID,
    resolution_note  TEXT
);

-- The reconcile job's own "is this account already flagged" check, and the
-- ops queue's default ?status=open listing.
CREATE INDEX idx_ledger_discrepancies_open ON ledger_discrepancies (account_id) WHERE resolved_at IS NULL;
