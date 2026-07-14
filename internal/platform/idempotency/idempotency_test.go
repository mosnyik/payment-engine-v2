package idempotency_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/idempotency"
)

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	_ = godotenv.Load("../../../.env")

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}

	pool, err := db.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestReserveNewThenInFlightThenCompleted(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	store := idempotency.New(pool)

	tenantID := uuid.New()
	key := "create-session-abc123"

	outcome, cached, err := store.Reserve(ctx, tenantID, key)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if outcome != idempotency.Claimed {
		t.Fatalf("expected New, got %v", outcome)
	}
	if cached != nil {
		t.Fatalf("expected no cached response on New, got %+v", cached)
	}

	// Second reservation before Complete() — simulates a concurrent duplicate request.
	outcome, cached, err = store.Reserve(ctx, tenantID, key)
	if err != nil {
		t.Fatalf("reserve (in-flight): %v", err)
	}
	if outcome != idempotency.InFlight {
		t.Fatalf("expected InFlight, got %v", outcome)
	}
	if cached != nil {
		t.Fatalf("expected no cached response while in-flight, got %+v", cached)
	}

	body := []byte(`{"session_id":"sess_123"}`)
	if err := store.Complete(ctx, tenantID, key, 201, body); err != nil {
		t.Fatalf("complete: %v", err)
	}

	outcome, cached, err = store.Reserve(ctx, tenantID, key)
	if err != nil {
		t.Fatalf("reserve (completed): %v", err)
	}
	if outcome != idempotency.Completed {
		t.Fatalf("expected Completed, got %v", outcome)
	}
	if cached == nil || cached.Code != 201 {
		t.Fatalf("unexpected cached response: %+v", cached)
	}
	// jsonb round-trips through Postgres's own formatting (e.g. adds a space
	// after ':'), so compare parsed values rather than raw bytes.
	var gotBody, wantBody map[string]any
	if err := json.Unmarshal(cached.Body, &gotBody); err != nil {
		t.Fatalf("unmarshal cached body: %v", err)
	}
	if err := json.Unmarshal(body, &wantBody); err != nil {
		t.Fatalf("unmarshal expected body: %v", err)
	}
	if gotBody["session_id"] != wantBody["session_id"] {
		t.Fatalf("body mismatch: got %v want %v", gotBody, wantBody)
	}
}

func TestReserveScopedPerTenant(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	store := idempotency.New(pool)

	key := "same-literal-key"
	tenantA := uuid.New()
	tenantB := uuid.New()

	outcomeA, _, err := store.Reserve(ctx, tenantA, key)
	if err != nil {
		t.Fatalf("reserve tenant A: %v", err)
	}
	if outcomeA != idempotency.Claimed {
		t.Fatalf("expected New for tenant A, got %v", outcomeA)
	}

	// Same literal key string, different tenant — must NOT collide.
	outcomeB, _, err := store.Reserve(ctx, tenantB, key)
	if err != nil {
		t.Fatalf("reserve tenant B: %v", err)
	}
	if outcomeB != idempotency.Claimed {
		t.Fatalf("expected New for tenant B (independent scope), got %v", outcomeB)
	}
}
