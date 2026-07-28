-- Phase 8's ledger.drift_detected (internal/ledger/reconcile.go) is the
-- first notification event that isn't tenant-scoped: the ledger account it's
-- about may be a platform/omnibus account (ledger_accounts.tenant_id is
-- already nullable for exactly this reason). tenant_id staying NOT NULL here
-- would make it impossible to ever page ops about a platform-account drift.
ALTER TABLE notification_deliveries ALTER COLUMN tenant_id DROP NOT NULL;
