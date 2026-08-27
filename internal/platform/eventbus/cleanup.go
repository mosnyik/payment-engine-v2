package eventbus

import (
	"context"
	"fmt"
	"log"
	"time"
)

// CleanupJob periodically deletes dispatched outbox_events rows older than
// retention. Migration 000001's own comment notes the dispatcher's partial
// index (WHERE dispatched_at IS NULL) keeps claiming cheap regardless of
// table size, but nothing ever removed a dispatched row — every published
// event accumulated forever (Phase 9's "outbox table cleanup" gap).
// Ticker-driven, same shape as every other background job in this codebase
// (e.g. session.TTLJob); pollInterval is how often the sweep runs, not how
// old a row must be — that's retention.
type CleanupJob struct {
	bus          *Bus
	retention    time.Duration
	pollInterval time.Duration
}

func NewCleanupJob(bus *Bus, retention, pollInterval time.Duration) *CleanupJob {
	return &CleanupJob{bus: bus, retention: retention, pollInterval: pollInterval}
}

// Run blocks until ctx is cancelled, sweeping every pollInterval.
func (j *CleanupJob) Run(ctx context.Context) {
	ticker := time.NewTicker(j.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.bus.deleteDispatchedBefore(ctx, time.Now().Add(-j.retention)); err != nil {
				log.Printf("eventbus: cleanup: %v", err)
			}
		}
	}
}

func (b *Bus) deleteDispatchedBefore(ctx context.Context, cutoff time.Time) error {
	_, err := b.pool.Exec(ctx,
		`DELETE FROM outbox_events WHERE dispatched_at IS NOT NULL AND dispatched_at < $1`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("eventbus: delete dispatched before %s: %w", cutoff, err)
	}
	return nil
}
