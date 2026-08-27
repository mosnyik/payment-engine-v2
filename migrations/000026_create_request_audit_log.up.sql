-- Blanket per-request audit log, ISP §7: every API request (except health
-- checks) gets a row here, independent of admin_audit_log (migration
-- 000003), which only covers admin actions a handler explicitly logs.
CREATE TABLE request_audit_log (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    request_id       TEXT NOT NULL,
    method           TEXT NOT NULL,
    path             TEXT NOT NULL,
    action           TEXT NOT NULL,
    resource_type    TEXT NULL,
    resource_id      TEXT NULL,
    client_ip        TEXT NOT NULL,
    user_agent       TEXT NOT NULL,
    body_hash        TEXT NOT NULL,
    status_code      INTEGER NOT NULL,
    response_time_ms BIGINT NOT NULL,
    tenant_id        UUID NULL,
    api_key_id       UUID NULL,
    admin_id         UUID NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Reviewed by tenant/key/admin during an incident, and purged on a rolling
-- window (ISP §7: 12 months) once that retention job exists (Phase 9).
CREATE INDEX idx_request_audit_log_tenant_id ON request_audit_log (tenant_id);
CREATE INDEX idx_request_audit_log_api_key_id ON request_audit_log (api_key_id);
CREATE INDEX idx_request_audit_log_admin_id ON request_audit_log (admin_id);
CREATE INDEX idx_request_audit_log_created_at ON request_audit_log (created_at);
