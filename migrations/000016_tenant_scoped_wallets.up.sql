-- Tenant-scoped HD wallets: every derived address and reservation now
-- belongs to a specific tenant's segregated HD account (see
-- internal/treasury/wallet's *AtAccount functions and hdwallet.go's
-- per-tenant account allocator) — not one shared platform-wide pool.
-- Existing rows predate tenant-scoping and have no tenant to attribute to;
-- this is still pre-production data (nothing has shipped), so they're
-- cleared rather than backfilled to a placeholder tenant.
TRUNCATE treasury_sweeps, treasury_deposits, treasury_address_reservations, derived_addresses, hd_wallet_indices CASCADE;

-- hd_wallet_indices: address-index allocation is now per (chain, tenant) —
-- different tenants have independent index sequences even on the same
-- chain, since they're different HD accounts.
ALTER TABLE hd_wallet_indices DROP CONSTRAINT hd_wallet_indices_pkey;
ALTER TABLE hd_wallet_indices ADD COLUMN tenant_id UUID NOT NULL;
ALTER TABLE hd_wallet_indices ADD PRIMARY KEY (chain, tenant_id);

-- derived_addresses: same per-tenant scoping for the address pool. Two
-- tenants can validly have the same (chain, derivation_index) pair — it's
-- a different HD account, hence a different address — only the raw
-- address string itself must stay globally unique.
ALTER TABLE derived_addresses DROP CONSTRAINT derived_addresses_chain_derivation_index_key;
ALTER TABLE derived_addresses ADD COLUMN tenant_id UUID NOT NULL;
ALTER TABLE derived_addresses ADD CONSTRAINT derived_addresses_chain_tenant_index_key UNIQUE (chain, tenant_id, derivation_index);

-- treasury_address_reservations: every reservation belongs to the tenant
-- whose session (Phase 5) or corridor entitlement it's for. Real FK to
-- tenants — same precedent as ledger_accounts.tenant_id (added once
-- tenants existed; no Go-level import needed for a SQL FK).
ALTER TABLE treasury_address_reservations ADD COLUMN tenant_id UUID NOT NULL REFERENCES tenants(id);

-- Tenant -> HD account-number assignment, made once per tenant, the first
-- time that tenant needs a self-custody address.
CREATE TABLE treasury_tenant_hd_accounts (
    tenant_id     UUID PRIMARY KEY,
    account_index BIGINT NOT NULL UNIQUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Singleton counter allocating the next tenant account number. Starts at
-- wallet.TenantAccountOffset (2) — accounts 0/1 are reserved for
-- platform-level deposit/gas-funding wallets (see wallet.go), never
-- assigned to a tenant.
CREATE TABLE treasury_hd_account_counter (
    id                 SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    next_account_index BIGINT NOT NULL DEFAULT 2
);

-- Tenant-supplied custom wallet addresses — monitored for deposits, never
-- swept (the platform holds no key for these; see
-- internal/treasury/tenant_wallet.go). One registered address per
-- (tenant, chain); reused across sessions the same way self-custody
-- addresses are, protected by the same partial unique index on
-- treasury_address_reservations(address) WHERE status='reserved'.
CREATE TABLE treasury_tenant_custom_wallets (
    tenant_id     UUID NOT NULL,
    chain         TEXT NOT NULL,
    address       TEXT NOT NULL,
    address_tag   TEXT NOT NULL DEFAULT '',
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, chain)
);
