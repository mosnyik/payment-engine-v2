-- Migration 000016 added CustodyTypeTenantProvided ('tenant_provided') in
-- Go but missed extending this CHECK constraint (created back in
-- migration 000010, before that custody type existed) to allow it.
ALTER TABLE treasury_address_reservations DROP CONSTRAINT treasury_address_reservations_custody_type_check;
ALTER TABLE treasury_address_reservations ADD CONSTRAINT treasury_address_reservations_custody_type_check
    CHECK (custody_type IN ('self_custody', 'partner_custodied', 'tenant_provided'));
