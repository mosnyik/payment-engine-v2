-- Hyparrow payout + Telegram manual-payout fallback (ARCHITECTURE.md §8
-- "Manual payout fallback").
--
-- awaiting_manual_payout: automatic payout gave up with nothing in flight,
-- and the payout details were posted to the ops Telegram group to pay by
-- hand. The session stays settling; ManualSettle moves this to settled.
ALTER TABLE settlements DROP CONSTRAINT settlements_status_check;
ALTER TABLE settlements ADD CONSTRAINT settlements_status_check CHECK (status IN (
    'pending_dispatch','dispatching','settling','settled',
    'settlement_failed','awaiting_manual_payout','reversed','reversal_resolved'));

-- failed_bucket3: a dispatched attempt a human confirmed genuinely failed
-- (confirmation timeout or an uncertain dispatch) before RetryPayout
-- re-dispatched — closed out so a late webhook for it is a no-op and its
-- ledger claim is reversed.
ALTER TABLE settlement_attempts DROP CONSTRAINT settlement_attempts_status_check;
ALTER TABLE settlement_attempts ADD CONSTRAINT settlement_attempts_status_check CHECK (status IN (
    'dispatched','succeeded','failed_bucket1','failed_bucket2','failed_bucket3','failed_bucket4'));
