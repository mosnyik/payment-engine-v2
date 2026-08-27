-- Self-service tenant login: passwordless, email magic-link, one login per
-- tenant company — distinct from tenant_api_keys (machine-to-machine HMAC
-- auth) and from admin_users (internal staff, password-based since that's
-- an internal, provisioned credential space, not a public one).
ALTER TABLE tenants
    ADD COLUMN contact_email TEXT UNIQUE;

-- A magic link is single-use (consumed_at) and short-lived (expires_at) —
-- redeeming one via tenant.VerifyMagicLink issues a real tenant_sessions
-- token below. Looked up by the hash of the opaque token mailed to the
-- tenant, never a raw value stored server-side.
CREATE TABLE tenant_magic_links (
    token_hash  TEXT PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ NULL
);

CREATE INDEX idx_tenant_magic_links_tenant_id ON tenant_magic_links (tenant_id);

-- The actual dashboard session, issued only after a magic link is redeemed.
-- Structurally identical to admin_sessions: looked up by the hash of an
-- opaque bearer token, never by comparing a secret string in application
-- code, same timing-attack-sidestepping reasoning as admin auth.
CREATE TABLE tenant_sessions (
    token_hash TEXT PRIMARY KEY,
    tenant_id  UUID NOT NULL REFERENCES tenants(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_tenant_sessions_tenant_id ON tenant_sessions (tenant_id);
