package settlement

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

// RegisterEventHandlers subscribes to session.deposit_confirmed — the
// fan-out point ARCHITECTURE.md §5 names settlement as an independent
// consumer of (not a pipeline behind treasury.swept). Call once, before the
// bus's dispatcher starts, same convention session.RegisterEventHandlers
// documents. A no-op if this Store has no bus.
func (s *Store) RegisterEventHandlers() {
	if s.bus == nil {
		return
	}
	s.bus.Subscribe("session.deposit_confirmed", s.handleDepositConfirmed)
}

// handleDepositConfirmed does nothing but claim a local row for
// DispatchWorker to pick up — per eventbus.Handler's doc comment, a handler
// must be a fast, local-DB-only write using the supplied tx; ledger.Post
// (which the real settlement work needs) always opens its own transaction
// and so can never run in here. e.AggregateID is the session id (see
// session/events.go's transitionByReservation, which publishes this event
// with AggregateID: sessionID).
func (s *Store) handleDepositConfirmed(ctx context.Context, tx pgx.Tx, e eventbus.Event) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO settlements (session_id, tenant_id, corridor_id, status, crypto_asset, fiat_currency)
		 SELECT id, tenant_id, corridor_id, 'pending_dispatch', crypto_asset, fiat_currency FROM sessions WHERE id = $1
		 ON CONFLICT (session_id) DO NOTHING`,
		e.AggregateID,
	)
	return err
}
