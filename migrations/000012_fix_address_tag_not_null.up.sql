-- treasury_address_reservations.address_tag is scanned into a plain Go
-- string (AddressReservation.AddressTag), never a *string — a NULL value
-- (possible today since the column has no default) would crash that scan.
-- Not just a hypothetical: any INSERT that lists a column subset omitting
-- address_tag leaves it NULL under the schema as originally created in
-- migration 000010. (provider_reference has the same issue — fixed
-- separately in migration 000013, discovered a moment after this one ran.)
UPDATE treasury_address_reservations SET address_tag = '' WHERE address_tag IS NULL;
ALTER TABLE treasury_address_reservations ALTER COLUMN address_tag SET DEFAULT '';
ALTER TABLE treasury_address_reservations ALTER COLUMN address_tag SET NOT NULL;
