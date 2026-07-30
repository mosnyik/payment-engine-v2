-- ARCHITECTURE.md §8 "Payout destination": per-session, not per-tenant — a
-- tenant can hold entitlements across multiple corridors/currencies at once,
-- so the destination is a property of the session, validated against the
-- corridor's own field requirements at CreateSession time.

-- Which destination fields a valid payout needs for this corridor's fiat
-- currency (e.g. an NGN corridor requires {account_number,bank_code}).
-- Empty array means "nothing required yet" — the default for every existing
-- corridor until an operator configures it, so this migration can't break
-- session creation on corridors nobody has updated.
ALTER TABLE corridors ADD COLUMN required_destination_fields TEXT[] NOT NULL DEFAULT '{}';

-- Tenant-supplied at CreateSession, validated against the corridor's
-- required_destination_fields immediately after the corridor lookup. Opaque
-- payload beyond that required-keys check (bank-detail shape varies by
-- corridor/provider), same convention settlement_attempts.provider_payload
-- already uses.
ALTER TABLE sessions ADD COLUMN payout_destination JSONB NOT NULL DEFAULT '{}';
