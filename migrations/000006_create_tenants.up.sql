-- tenant = the integrating bank/fintech. Status starts at pending_kyb; no
-- API credentials can be issued until compliance approves (see
-- tenant.ApproveKYB and the onboarding workflow in ARCHITECTURE.md §5).
CREATE TABLE tenants (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending_kyb' CHECK (status IN ('pending_kyb', 'active', 'suspended')),
    fee_bps        INT NOT NULL DEFAULT 100,  -- basis points; per-corridor override lives on the entitlement row
    webhook_url    TEXT NULL,                 -- SSRF-validated at write time, see tenant.ValidateWebhookURL
    webhook_secret TEXT NULL,                 -- signs outbound webhooks TO the tenant; distinct from their inbound HMAC auth secret
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- HMAC secret is encrypted at rest (internal/platform/crypto), not hashed —
-- unlike a password, the server must recover the raw value to compute a
-- signature to compare against. auth_method is per-tenant configurable
-- (ARCHITECTURE.md decision); mtls_cert_fingerprint is stored so a tenant
-- CAN be configured for it, even though gateway's mTLS verification itself
-- isn't wired yet (deferred until this table existed to express the choice
-- — see internal/platform/gateway/router.go).
CREATE TABLE tenant_api_keys (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants(id),
    api_key               TEXT NOT NULL UNIQUE,
    hmac_secret_encrypted TEXT NOT NULL,
    auth_method           TEXT NOT NULL DEFAULT 'hmac' CHECK (auth_method IN ('hmac', 'mtls')),
    mtls_cert_fingerprint TEXT NULL,
    active                BOOLEAN NOT NULL DEFAULT true,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at            TIMESTAMPTZ NULL
);

CREATE INDEX idx_tenant_api_keys_tenant_id ON tenant_api_keys (tenant_id);

-- Which corridors a tenant may use, and any per-corridor fee override.
CREATE TABLE tenant_corridor_entitlements (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    corridor_id      UUID NOT NULL REFERENCES corridors(id),
    active           BOOLEAN NOT NULL DEFAULT true,
    fee_bps_override INT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, corridor_id)
);
