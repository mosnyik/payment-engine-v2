-- Inbound-request idempotency: "did the tenant already POST this request".
-- Distinct from the ledger's own idempotency keys (internal money-movement
-- claims) — this one guards the API boundary.
CREATE TABLE idempotency_keys (
    tenant_id     UUID NOT NULL,
    key           TEXT NOT NULL,
    response_code INT NULL,
    response_body JSONB NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);
