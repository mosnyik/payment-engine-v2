-- 2Settle charges no fees; its margin is the rate spread built into every
-- rate lock (internal/rate LockRate). The fee columns are kept but nothing
-- reads or writes them any more. Default the tenant column to 0 so new rows
-- don't suggest a 1% fee that is never applied.
ALTER TABLE tenants ALTER COLUMN fee_bps SET DEFAULT 0;
