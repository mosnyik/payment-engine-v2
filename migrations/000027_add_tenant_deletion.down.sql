ALTER TABLE tenants DROP COLUMN deleted_at;

ALTER TABLE tenants DROP CONSTRAINT tenants_status_check;
ALTER TABLE tenants ADD CONSTRAINT tenants_status_check
    CHECK (status IN ('pending_kyb', 'active', 'suspended'));
