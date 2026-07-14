-- Separate credential space for staff (KYB review, compliance holds,
-- corridor config, manual settlement retry) — never the tenant gateway's
-- HMAC/mTLS auth, and never a single shared secret (that was the v1
-- audit finding this replaces: one ADMIN_SECRET, non-constant-time compare,
-- no per-admin identity, no attempt limiting).
CREATE TABLE admin_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    disabled_at   TIMESTAMPTZ NULL
);

-- Sessions are looked up by the hash of an opaque bearer token, never by
-- comparing a secret string in application code — this sidesteps timing
-- attacks structurally rather than relying on a constant-time compare.
CREATE TABLE admin_sessions (
    token_hash TEXT PRIMARY KEY,
    admin_id   UUID NOT NULL REFERENCES admin_users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_admin_sessions_admin_id ON admin_sessions (admin_id);

-- Every sensitive admin action gets a row here: who did what, to what.
CREATE TABLE admin_audit_log (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_id   UUID NOT NULL REFERENCES admin_users(id),
    action     TEXT NOT NULL,
    target     TEXT NULL,
    metadata   JSONB NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_admin_audit_log_admin_id ON admin_audit_log (admin_id);
