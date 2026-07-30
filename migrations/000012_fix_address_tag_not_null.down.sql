ALTER TABLE treasury_address_reservations ALTER COLUMN address_tag DROP NOT NULL;
ALTER TABLE treasury_address_reservations ALTER COLUMN address_tag DROP DEFAULT;
