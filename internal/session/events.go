package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

// RegisterEventHandlers subscribes to the treasury events the deposit-side
// transitions below depend on. Call once, before the bus's dispatcher
// starts — eventbus.Subscribe's own doc comment says registering
// concurrently with dispatch isn't safe. A no-op if this Store has no bus
// (see the bus field's doc comment in session.go).
func (s *Store) RegisterEventHandlers() {
	if s.bus == nil {
		return
	}
	s.bus.Subscribe("treasury.deposit_detected", s.handleDepositDetected)
	s.bus.Subscribe("treasury.deposit_confirmed", s.handleDepositConfirmed)
}

func (s *Store) handleDepositDetected(ctx context.Context, tx pgx.Tx, e eventbus.Event) error {
	return s.transitionByReservation(ctx, tx, e.AggregateID, "pending", "deposit_detected", "session.deposit_detected")
}

// handleDepositConfirmed drives pending's fan-out point (ARCHITECTURE.md
// §8): deposit_confirmed is published for a future settlement/treasury-
// sweep consumer, same "config/Store-level only, no caller yet" precedent
// already used for rate/corridor/Busha in earlier phases. No settling
// transition is attempted here — settlement (Phase 6) doesn't exist yet.
func (s *Store) handleDepositConfirmed(ctx context.Context, tx pgx.Tx, e eventbus.Event) error {
	return s.transitionByReservation(ctx, tx, e.AggregateID, "deposit_detected", "deposit_confirmed", "session.deposit_confirmed")
}

// transitionByReservation CAS-transitions the session owning reservationID
// from -> to and publishes eventType in the same handler transaction. A
// reservation with no session currently in the expected state (already
// moved on, or a redelivered/duplicate treasury event) is not an error —
// eventbus.Handler's doc comment requires handlers be safe no-ops on
// redelivery, not just idempotent writes.
func (s *Store) transitionByReservation(ctx context.Context, tx pgx.Tx, reservationID uuid.UUID, from, to, eventType string) error {
	var sessionID uuid.UUID
	err := tx.QueryRow(ctx,
		`UPDATE sessions SET status = $3, updated_at = now()
		 WHERE deposit_reservation_id = $1 AND status = $2
		 RETURNING id`,
		reservationID, from, to,
	).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("session: transition by reservation: %w", err)
	}

	if s.bus == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]string{"reservation_id": reservationID.String()})
	if err != nil {
		return fmt.Errorf("session: marshal %s payload: %w", eventType, err)
	}
	if err := s.bus.Publish(ctx, tx, eventbus.Event{
		EventType:     eventType,
		AggregateType: "session",
		AggregateID:   sessionID,
		Payload:       payload,
	}); err != nil {
		return fmt.Errorf("session: publish %s: %w", eventType, err)
	}
	return nil
}
