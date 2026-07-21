-- Tracks whether a confirmed deposit has already been included in a sweep,
-- separately from treasury_deposits.status (which stays 'confirmed' —
-- sweeping doesn't change the deposit's own state machine, it's a
-- different concern: treasury_sweeps is the outbound side, this just
-- marks a confirmed deposit as already accounted for by one).
ALTER TABLE treasury_deposits ADD COLUMN swept_at TIMESTAMPTZ;
