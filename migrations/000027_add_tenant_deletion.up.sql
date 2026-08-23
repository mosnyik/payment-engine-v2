-- Self-service account deletion: disables the tenant (no portal login, no
-- HMAC auth) while the row itself, and everything it references, is kept
-- for the 5-year AML/CFT retention window ISP §7 specifies — "delete" here
-- means access revocation, never data removal. Distinct from `suspended`
-- (an admin/compliance action) so tenant status still answers "why is this
-- account inactive" unambiguously.
ALTER TABLE tenants DROP CONSTRAINT tenants_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_status_check
    CHECK (status IN ('pending_kyb', 'active', 'suspended', 'deleted'));

ALTER TABLE tenants ADD COLUMN deleted_at TIMESTAMPTZ NULL;
