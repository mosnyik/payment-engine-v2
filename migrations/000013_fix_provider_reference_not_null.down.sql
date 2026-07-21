ALTER TABLE treasury_address_reservations ALTER COLUMN provider_reference DROP NOT NULL;
ALTER TABLE treasury_address_reservations ALTER COLUMN provider_reference DROP DEFAULT;
