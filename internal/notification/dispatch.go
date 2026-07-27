package notification

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DispatchWorker is the ticker-driven sender — same shape as
// settlement.DispatchWorker/session.TTLJob, for the same reason
// (eventbus.Handler must be a fast, local-DB-only write; an HTTP/SMTP call
// must never run inside its transaction).
type DispatchWorker struct {
	store        *Store
	pollInterval time.Duration
}

func NewDispatchWorker(store *Store, pollInterval time.Duration) *DispatchWorker {
	return &DispatchWorker{store: store, pollInterval: pollInterval}
}

// Run blocks until ctx is cancelled, same shape as every other background
// job in this codebase.
func (w *DispatchWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.dispatchBatch(ctx)
		}
	}
}

// dispatchBatch drains every currently-due delivery in one tick — same
// "single small table, CAS claim is sufficient" reasoning
// settlement.DispatchWorker.dispatchBatch's doc comment gives.
func (w *DispatchWorker) dispatchBatch(ctx context.Context) {
	for {
		claimed, err := w.store.claimNextPendingDispatch(ctx)
		if err != nil {
			log.Printf("notification: dispatch batch: %v", err)
			return
		}
		if claimed == nil {
			return
		}
		if err := w.store.processDelivery(ctx, claimed); err != nil {
			log.Printf("notification: process delivery %s: %v", claimed.ID, err)
		}
	}
}

// claimNextPendingDispatch atomically claims the oldest due delivery by
// pushing it 5 minutes out of the claimable window — a lightweight
// equivalent of settlement's 'dispatching' intermediate status using the
// columns this table already has, so a slow send can't be double-claimed by
// the next tick. processDelivery always overwrites next_attempt_at with the
// real outcome (delivered/backoff/dead_letter) before this placeholder
// would ever matter.
func (s *Store) claimNextPendingDispatch(ctx context.Context) (*Delivery, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE notification_deliveries SET next_attempt_at = now() + interval '5 minutes', updated_at = now()
		 WHERE id = (
		   SELECT id FROM notification_deliveries
		   WHERE status = 'pending_dispatch' AND next_attempt_at <= now()
		   ORDER BY next_attempt_at LIMIT 1
		 )
		 RETURNING `+deliveryColumns,
	)
	d, err := scanDelivery(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("notification: claim next pending dispatch: %w", err)
	}
	return d, nil
}

// processDelivery sends d via its channel and records the outcome. Email's
// disabled-provider case dead-letters immediately rather than working
// through the full backoff ladder first — a disabled provider fails
// identically on every attempt, so retrying it is pure noise; it's the
// static-config equivalent of settlement's bucket-4 "terminal, no retry".
func (s *Store) processDelivery(ctx context.Context, d *Delivery) error {
	if d.Channel == ChannelEmail && !s.emailProvider.IsEnabled() {
		return s.deadLetter(ctx, d, "email provider not configured")
	}

	var sendErr error
	switch d.Channel {
	case ChannelWebhook:
		sendErr = s.sendWebhook(ctx, d)
	case ChannelEmail:
		sendErr = s.emailProvider.Send(ctx, d.Destination, d.EventType, string(d.Payload))
	default:
		return fmt.Errorf("notification: unknown channel %q", d.Channel)
	}

	if sendErr == nil {
		return s.markDelivered(ctx, d.ID)
	}
	return s.handleDeliveryFailure(ctx, d, sendErr)
}

func (s *Store) markDelivered(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE notification_deliveries SET status = 'delivered', delivered_at = now(), updated_at = now() WHERE id = $1`,
		id,
	)
	if err != nil {
		return fmt.Errorf("notification: mark delivered: %w", err)
	}
	return nil
}

// handleDeliveryFailure advances attempt_count and either schedules the
// next backoff step or dead-letters once the schedule is exhausted.
func (s *Store) handleDeliveryFailure(ctx context.Context, d *Delivery, sendErr error) error {
	attempt := d.AttemptCount + 1
	if attempt > len(backoffSteps) {
		return s.deadLetter(ctx, d, sendErr.Error())
	}
	nextAttemptAt := time.Now().Add(backoffSteps[attempt-1])
	_, err := s.pool.Exec(ctx,
		`UPDATE notification_deliveries SET status = 'pending_dispatch', attempt_count = $2, next_attempt_at = $3, last_error = $4, updated_at = now() WHERE id = $1`,
		d.ID, attempt, nextAttemptAt, sendErr.Error(),
	)
	if err != nil {
		return fmt.Errorf("notification: schedule retry: %w", err)
	}
	return nil
}

func (s *Store) deadLetter(ctx context.Context, d *Delivery, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE notification_deliveries SET status = 'dead_letter', attempt_count = attempt_count + 1, last_error = $2, updated_at = now() WHERE id = $1`,
		d.ID, reason,
	)
	if err != nil {
		return fmt.Errorf("notification: dead letter: %w", err)
	}
	log.Printf("notification: delivery %s dead-lettered: %s", d.ID, reason)
	return nil
}
