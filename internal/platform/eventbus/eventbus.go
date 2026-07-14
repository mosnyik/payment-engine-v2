// Package eventbus is the transactional-outbox event dispatcher used for
// fan-out between modules. Publish writes an outbox row as part of the
// caller's own transaction, so the event can never be lost or duplicated
// relative to the state change it describes. A dispatcher claims rows with
// SELECT ... FOR UPDATE SKIP LOCKED and invokes subscribed handlers.
//
// This is in-process today; swapping it for a real broker (NATS/Kafka) at
// microservice-split time changes this package's internals, not any
// Publish/Subscribe call site elsewhere in the codebase.
package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

// Event is a fact published by one module for other modules to react to.
type Event struct {
	ID            uuid.UUID
	EventType     string
	AggregateType string
	AggregateID   uuid.UUID
	Payload       json.RawMessage
	CreatedAt     time.Time
}

// Handler reacts to an event. Handlers must be fast, local DB-only writes
// using the supplied tx — claim, handler execution, and marking the event
// dispatched all happen inside one transaction. A handler that needs to
// make a slow or external call (a provider API, etc.) should instead write
// local state a dedicated worker picks up, not make the call inline —
// otherwise it would hold the claim's row lock open for the call's duration.
//
// Handlers must be idempotent: a handler failure rolls back the whole
// transaction (including the dispatched-mark), so the event is redelivered
// on the next tick. Redelivery is the intended recovery path, not a
// special case — this is exactly why the ledger's Post() requires an
// idempotency key on every write.
type Handler func(ctx context.Context, tx pgx.Tx, event Event) error

// Bus is the in-process event dispatcher backed by the outbox_events table.
type Bus struct {
	pool *db.Pool

	mu       sync.RWMutex
	handlers map[string][]Handler

	wake chan struct{}
}

func New(pool *db.Pool) *Bus {
	return &Bus{
		pool:     pool,
		handlers: make(map[string][]Handler),
		wake:     make(chan struct{}, 1),
	}
}

// Subscribe registers h to run whenever an event of eventType is dispatched.
// Call before Run starts — subscriptions are not safe to add concurrently
// with dispatch.
func (b *Bus) Subscribe(eventType string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
}

// Publish records an event in the outbox as part of tx. The caller MUST
// commit tx together with the state change the event describes — that
// shared transaction is what makes the event and the state change atomic.
func (b *Bus) Publish(ctx context.Context, tx pgx.Tx, event Event) error {
	payload := event.Payload
	if payload == nil {
		payload = json.RawMessage("{}")
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO outbox_events (event_type, aggregate_type, aggregate_id, payload)
		 VALUES ($1, $2, $3, $4)`,
		event.EventType, event.AggregateType, event.AggregateID, payload,
	)
	if err != nil {
		return fmt.Errorf("eventbus: publish: %w", err)
	}
	return nil
}

// Notify wakes the dispatcher immediately instead of waiting for the next
// poll tick. Non-blocking — safe to call right after a commit on a request
// path. The poll tick remains as a reliability backstop for anything missed
// across a crash or restart, not the primary dispatch path.
func (b *Bus) Notify() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Run starts the dispatcher loop. Blocks until ctx is cancelled.
func (b *Bus) Run(ctx context.Context, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			b.dispatchBatch(ctx)
		case <-b.wake:
			b.dispatchBatch(ctx)
		}
	}
}

const batchSize = 50

// dispatchBatch claims and processes up to batchSize events, one per
// transaction so a single failing handler only blocks its own event, not
// the rest of the batch.
func (b *Bus) dispatchBatch(ctx context.Context) {
	for i := 0; i < batchSize; i++ {
		dispatched, err := b.dispatchOne(ctx)
		if err != nil {
			log.Printf("eventbus: %v", err)
			return
		}
		if !dispatched {
			return
		}
	}
}

func (b *Bus) dispatchOne(ctx context.Context) (bool, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	var e Event
	err = tx.QueryRow(ctx,
		`SELECT id, event_type, aggregate_type, aggregate_id, payload, created_at
		 FROM outbox_events
		 WHERE dispatched_at IS NULL
		 ORDER BY created_at
		 LIMIT 1
		 FOR UPDATE SKIP LOCKED`,
	).Scan(&e.ID, &e.EventType, &e.AggregateType, &e.AggregateID, &e.Payload, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}

	b.mu.RLock()
	handlers := b.handlers[e.EventType]
	b.mu.RUnlock()

	for _, h := range handlers {
		if err := h(ctx, tx, e); err != nil {
			return false, fmt.Errorf("handler for %s: %w", e.EventType, err)
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE outbox_events SET dispatched_at = now(), attempts = attempts + 1 WHERE id = $1`,
		e.ID,
	); err != nil {
		return false, fmt.Errorf("mark dispatched: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}

	return true, nil
}
