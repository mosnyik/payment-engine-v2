-- KYC as a service (POST /v2/kyc/verify): one row per identity check a
-- tenant runs through its API key. This is the usage/billing record and
-- audit trail — it deliberately holds no personal data. The ID number is
-- stored only as a keyed hash (id_number_hash, HMAC-SHA256 with a server
-- key, so an 11-digit BVN can't be brute-forced back out of it), and
-- nothing the provider returned (name, DOB, phone, address, photo) is kept:
-- only the per-field match outcome in checks.
CREATE TABLE kyc_checks (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES tenants (id),
    api_key_id     UUID NULL REFERENCES tenant_api_keys (id),
    provider       TEXT NOT NULL,
    id_type        TEXT NOT NULL CHECK (id_type IN ('bvn', 'nin', 'phone', 'drivers_license')),
    id_number_hash TEXT NOT NULL,
    result         TEXT NOT NULL CHECK (result IN ('match', 'partial_match', 'no_match', 'not_found', 'error')),
    checks         JSONB NOT NULL DEFAULT '{}',
    -- What the provider billed us for this call, in provider_fee_currency,
    -- when it reported one. NULL for sandbox and failed calls.
    provider_fee          NUMERIC(12, 2) NULL,
    provider_fee_currency TEXT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Per-tenant usage over a billing period.
CREATE INDEX idx_kyc_checks_tenant_created ON kyc_checks (tenant_id, created_at);
