-- Follow-up to 000004_create_ledger.up.sql, which deliberately left
-- ledger_accounts.tenant_id without a foreign key because `tenants` didn't
-- exist yet. It does now (000006) — add the constraint.
ALTER TABLE ledger_accounts
    ADD CONSTRAINT ledger_accounts_tenant_id_fkey
    FOREIGN KEY (tenant_id) REFERENCES tenants(id);
