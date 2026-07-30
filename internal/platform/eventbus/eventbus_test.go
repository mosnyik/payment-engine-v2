package eventbus_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	_ = godotenv.Load("../../../.env")

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}

	pool, err := db.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPublishAndDispatch(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	bus := eventbus.New(pool, 50)

	received := make(chan eventbus.Event, 1)
	bus.Subscribe("test.thing_happened", func(ctx context.Context, tx pgx.Tx, e eventbus.Event) error {
		received <- e
		return nil
	})

	aggID := uuid.New()
	payload, _ := json.Marshal(map[string]string{"hello": "world"})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := bus.Publish(ctx, tx, eventbus.Event{
		EventType:     "test.thing_happened",
		AggregateType: "test",
		AggregateID:   aggID,
		Payload:       payload,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go bus.Run(runCtx, 50*time.Millisecond)

	select {
	case e := <-received:
		if e.AggregateID != aggID {
			t.Fatalf("aggregate id mismatch: got %s want %s", e.AggregateID, aggID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler was not invoked within timeout")
	}

	var dispatchedAt *time.Time
	err = pool.QueryRow(ctx, `SELECT dispatched_at FROM outbox_events WHERE aggregate_id = $1`, aggID).Scan(&dispatchedAt)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if dispatchedAt == nil {
		t.Fatal("expected dispatched_at to be set")
	}
}

func TestHandlerFailureDoesNotMarkDispatched(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	bus := eventbus.New(pool, 50)

	attempts := 0
	done := make(chan struct{})
	bus.Subscribe("test.flaky", func(ctx context.Context, tx pgx.Tx, e eventbus.Event) error {
		attempts++
		if attempts < 2 {
			return context.DeadlineExceeded // simulate a transient failure
		}
		close(done)
		return nil
	})

	aggID := uuid.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := bus.Publish(ctx, tx, eventbus.Event{
		EventType:     "test.flaky",
		AggregateType: "test",
		AggregateID:   aggID,
		Payload:       json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go bus.Run(runCtx, 50*time.Millisecond)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("expected retry to succeed, got %d attempts", attempts)
	}

	if attempts < 2 {
		t.Fatalf("expected at least 2 attempts (one failure, one success), got %d", attempts)
	}
}
