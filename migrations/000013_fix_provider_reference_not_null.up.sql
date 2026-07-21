-- Same issue as migration 000012, for provider_reference (missed in that
-- pass) — see its comment for the full rationale.
UPDATE treasury_address_reservations SET provider_reference = '' WHERE provider_reference IS NULL;
ALTER TABLE treasury_address_reservations ALTER COLUMN provider_reference SET DEFAULT '';
ALTER TABLE treasury_address_reservations ALTER COLUMN provider_reference SET NOT NULL;
