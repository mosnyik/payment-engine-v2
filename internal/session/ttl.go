package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

// TTLJob is the background sweep implementing ARCHITECTURE.md §8's
// TTL/SLA rules. Shaped like eventbus.Bus.Run/rate.FetchJob.Run — started
// with `go job.Run(ctx)` from main.go.
type TTLJob struct {
	store        *Store
	pollInterval time.Duration
}

func NewTTLJob(store *Store, pollInterval time.Duration) *TTLJob {
	return &TTLJob{store: store, pollInterval: pollInterval}
}

// Run blocks until ctx is cancelled, sweeping every pollInterval.
func (j *TTLJob) Run(ctx context.Context) {
	ticker := time.NewTicker(j.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.sweepOnce(ctx); err != nil {
				log.Printf("session: ttl sweep: %v", err)
			}
		}
	}
}

func (j *TTLJob) sweepOnce(ctx context.Context) error {
	if err := j.store.expirePreDeposit(ctx); err != nil {
		return fmt.Errorf("expire pre-deposit: %w", err)
	}
	if err := j.store.expireTimedOutHolds(ctx); err != nil {
		return fmt.Errorf("expire timed-out holds: %w", err)
	}
	if err := j.store.markSLABreaches(ctx); err != nil {
		return fmt.Errorf("mark sla breaches: %w", err)
	}
	return nil
}

// expirePreDeposit expires screening/pending sessions whose fixed 30-minute
// TTL has elapsed with no deposit ever detected — the one case
// ARCHITECTURE.md §8 says is safe to abandon. compliance_hold is
// deliberately not included here: its own corridor-configured timeout
// (expireTimedOutHolds, below) governs it, not this fixed clock — a hold
// review often legitimately outlives 30 minutes.
func (s *Store) expirePreDeposit(ctx context.Context) error {
	cutoff := time.Now().Add(-SessionTTL)
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET status = 'expired', updated_at = now()
		 WHERE status IN ('screening', 'pending') AND created_at < $1`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("session: expire pre-deposit: %w", err)
	}
	return nil
}

// expireTimedOutHolds expires compliance_hold sessions whose case has
// passed its corridor-configured hold_expires_at. The compliance case
// itself is untouched (stays 'hold' in compliance_cases) — session expiry
// must never silently close the underlying case, per §8.
func (s *Store) expireTimedOutHolds(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions s SET status = 'expired', updated_at = now()
		 FROM compliance_cases c
		 WHERE s.compliance_case_id = c.id
		   AND s.status = 'compliance_hold'
		   AND c.hold_expires_at IS NOT NULL
		   AND c.hold_expires_at < now()`,
	)
	if err != nil {
		return fmt.Errorf("session: expire timed-out holds: %w", err)
	}
	return nil
}

// markSLABreaches sets sla_breached_at once for any session still in
// flight (not settled/expired/rejected) past the 30-minute mark. Status is
// never touched here — a session with a deposit already detected keeps
// progressing through its real pipeline exactly as normal after breaching,
// per §8 rule 1.
//
// Runs inside a transaction and publishes session.sla_breached once per
// breaching session, in the same transaction as the UPDATE — same atomicity
// discipline transitionByReservation (events.go) already uses. A bulk
// UPDATE ... RETURNING can return one row per affected session, so no
// separate SELECT ... FOR UPDATE pass is needed first. The
// `sla_breached_at IS NULL` clause is what keeps a redelivered/duplicate
// sweep from re-publishing for a session already marked.
func (s *Store) markSLABreaches(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("session: mark sla breaches: begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	cutoff := time.Now().Add(-SessionTTL)
	rows, err := tx.Query(ctx,
		`UPDATE sessions SET sla_breached_at = now()
		 WHERE sla_breached_at IS NULL
		   AND status NOT IN ('settled', 'expired', 'rejected')
		   AND created_at < $1
		 RETURNING id, tenant_id`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("session: mark sla breaches: %w", err)
	}
	type breach struct {
		sessionID uuid.UUID
		tenantID  uuid.UUID
	}
	var breached []breach
	for rows.Next() {
		var b breach
		if err := rows.Scan(&b.sessionID, &b.tenantID); err != nil {
			rows.Close()
			return fmt.Errorf("session: mark sla breaches: scan: %w", err)
		}
		breached = append(breached, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("session: mark sla breaches: %w", err)
	}

	if s.bus != nil {
		for _, b := range breached {
			payload, err := json.Marshal(map[string]string{
				"tenant_id": b.tenantID.String(),
			})
			if err != nil {
				return fmt.Errorf("session: marshal session.sla_breached payload: %w", err)
			}
			if err := s.bus.Publish(ctx, tx, eventbus.Event{
				EventType:     "session.sla_breached",
				AggregateType: "session",
				AggregateID:   b.sessionID,
				Payload:       payload,
			}); err != nil {
				return fmt.Errorf("session: publish session.sla_breached: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("session: mark sla breaches: commit: %w", err)
	}
	return nil
}
