package settlement

import (
	"context"
	"fmt"
	"log"
	"time"
)

// TimeoutJob detects bucket 3 (confirmation timeout, ARCHITECTURE.md §8):
// a dispatched attempt whose provider never sent a success/failure webhook
// within ConfirmationTimeout. Shaped like session.TTLJob/rate.FetchJob.
//
// It never touches settlements.status — §8 is explicit that bucket 3 "stays
// settling" pending a human's manual verification with the provider. This
// job's only effect is setting ops_paged_at so the ops queue surfaces it;
// the actual retry only happens via Store.RetryPayout once a human has
// confirmed the original attempt genuinely failed (ops.go).
type TimeoutJob struct {
	store        *Store
	pollInterval time.Duration
}

func NewTimeoutJob(store *Store, pollInterval time.Duration) *TimeoutJob {
	return &TimeoutJob{store: store, pollInterval: pollInterval}
}

func (j *TimeoutJob) Run(ctx context.Context) {
	ticker := time.NewTicker(j.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.sweepOnce(ctx); err != nil {
				log.Printf("settlement: timeout sweep: %v", err)
			}
		}
	}
}

func (j *TimeoutJob) sweepOnce(ctx context.Context) error {
	if err := j.store.pageOpsForTimedOutConfirmations(ctx); err != nil {
		return fmt.Errorf("page ops for timed-out confirmations: %w", err)
	}
	return nil
}

func (s *Store) pageOpsForTimedOutConfirmations(ctx context.Context) error {
	cutoff := time.Now().Add(-ConfirmationTimeout)
	_, err := s.pool.Exec(ctx,
		`UPDATE settlements SET ops_paged_at = now()
		 WHERE status = 'settling' AND ops_paged_at IS NULL AND updated_at < $1
		   AND EXISTS (
		     SELECT 1 FROM settlement_attempts a
		     WHERE a.settlement_id = settlements.id AND a.status = 'dispatched' AND a.dispatched_at < $1
		   )`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("settlement: page ops for timed-out confirmations: %w", err)
	}
	return nil
}
