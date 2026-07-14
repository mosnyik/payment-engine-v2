-- One table serves both KYB (tenant onboarding) and, later, transaction
-- screening (Phase 5's session module) — case_type + a polymorphic
-- reference, same pattern as ledger_transactions' reference_type/id.
--
-- status defaults to 'hold': no screening vendor is selected yet (project
-- decision), so every case that isn't screened by an explicitly-named
-- provider lands in the manual hold queue. Automated providers slot in
-- later via the in-code Registry — this table and its callers don't change
-- when one is added.
--
-- hold_expires_at is nullable and NOT populated by KYB screening (a
-- business KYB review has no natural session-like TTL — it sits in queue
-- until an admin acts). It exists for Phase 5's transaction-screening
-- holds, which DO have a corridor-configured timeout
-- (corridors.compliance_hold_timeout_seconds) and will populate it.
CREATE TABLE compliance_cases (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    case_type       TEXT NOT NULL CHECK (case_type IN ('kyb', 'transaction_screening')),
    reference_type  TEXT NOT NULL,
    reference_id    UUID NOT NULL,
    status          TEXT NOT NULL DEFAULT 'hold' CHECK (status IN ('approved', 'rejected', 'hold')),
    provider_name   TEXT NULL,
    decision_reason TEXT NULL,
    submitted_data  JSONB NOT NULL DEFAULT '{}',
    hold_expires_at TIMESTAMPTZ NULL,
    resolved_by     UUID NULL REFERENCES admin_users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ NULL
);

CREATE INDEX idx_compliance_cases_reference ON compliance_cases (reference_type, reference_id);
CREATE INDEX idx_compliance_cases_hold_queue ON compliance_cases (created_at) WHERE status = 'hold';
