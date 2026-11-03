-- Fails if any row still uses a status introduced by the up migration —
-- resolve those rows first rather than silently rewriting them.
ALTER TABLE settlement_attempts DROP CONSTRAINT settlement_attempts_status_check;
ALTER TABLE settlement_attempts ADD CONSTRAINT settlement_attempts_status_check CHECK (status IN (
    'dispatched','succeeded','failed_bucket1','failed_bucket2','failed_bucket4'));

ALTER TABLE settlements DROP CONSTRAINT settlements_status_check;
ALTER TABLE settlements ADD CONSTRAINT settlements_status_check CHECK (status IN (
    'pending_dispatch','dispatching','settling','settled',
    'settlement_failed','reversed','reversal_resolved'));
