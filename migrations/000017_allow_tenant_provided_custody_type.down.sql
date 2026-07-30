ALTER TABLE treasury_address_reservations DROP CONSTRAINT treasury_address_reservations_custody_type_check;
ALTER TABLE treasury_address_reservations ADD CONSTRAINT treasury_address_reservations_custody_type_check
    CHECK (custody_type IN ('self_custody', 'partner_custodied'));
