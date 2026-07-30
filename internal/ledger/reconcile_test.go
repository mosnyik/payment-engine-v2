package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/ledger"
)

// runReconcileSweep runs one burst of ledger.ReconcileJob against a short
// poll interval, giving it enough time for at least one tick, then stops it
// — same convention session.TTLJob's own tests establish.
func runReconcileSweep(t *testing.T, l *ledger.Ledger) {
	t.Helper()
	job := ledger.NewReconcileJob(l, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	job.Run(ctx)
}

func findDiscrepancy(discrepancies []ledger.Discrepancy, accountID uuid.UUID) *ledger.Discrepancy {
	for i := range discrepancies {
		if discrepancies[i].AccountID == accountID {
			return &discrepancies[i]
		}
	}
	return nil
}

func TestReconcile_FlagsDriftBetweenCachedAndComputedBalance(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	l := ledger.New(pool)

	a, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_reconcile_a"), "USD", "fiat", "A")
	must(t, err)
	b, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_reconcile_b"), "USD", "fiat", "B")
	must(t, err)

	_, err = l.Post(ctx, ledger.Transaction{
		IdempotencyKey: uuid.NewString(),
		TxnType:        "manual_adjustment",
		ReferenceType:  "test",
		ReferenceID:    uuid.New(),
		CreatedBy:      "test",
		Entries: []ledger.Entry{
			{AccountID: a, Direction: ledger.Debit, Amount: d("50"), AssetCode: "USD"},
			{AccountID: b, Direction: ledger.Credit, Amount: d("50"), AssetCode: "USD"},
		},
	})
	must(t, err)

	// Post can never itself produce drift (cache update happens in the same
	// transaction, atomically) — simulate the only realistic external cause,
	// direct corruption of the cache.
	_, err = pool.Exec(ctx, `UPDATE ledger_balances SET balance = balance + 10 WHERE account_id = $1`, a)
	must(t, err)

	runReconcileSweep(t, l)

	open, err := l.ListDiscrepancies(ctx, false)
	must(t, err)
	found := findDiscrepancy(open, a)
	if found == nil {
		t.Fatalf("expected an open discrepancy for account %s", a)
	}
	if !found.DriftAmount.Equal(d("10")) {
		t.Fatalf("expected drift_amount 10, got %s", found.DriftAmount)
	}
	if !found.CachedBalance.Equal(d("60")) {
		t.Fatalf("expected cached_balance 60, got %s", found.CachedBalance)
	}
	if !found.ComputedBalance.Equal(d("50")) {
		t.Fatalf("expected computed_balance 50, got %s", found.ComputedBalance)
	}

	// A second sweep must not open a duplicate row for the same
	// still-unresolved drift — otherwise every poll interval would re-page
	// ops about the same still-broken account forever.
	runReconcileSweep(t, l)
	open2, err := l.ListDiscrepancies(ctx, false)
	must(t, err)
	count := 0
	for _, disc := range open2 {
		if disc.AccountID == a {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one open discrepancy after a second sweep, got %d", count)
	}

	// Resolving it, then sweeping again with the same underlying drift still
	// present, must open a NEW row — resolving only closes that specific
	// finding, it never suppresses a still-real problem.
	resolved, err := l.ResolveDiscrepancy(ctx, found.ID, uuid.New(), "investigated, filing separately")
	must(t, err)
	if resolved.ResolvedAt == nil {
		t.Fatal("expected resolved_at to be set")
	}

	runReconcileSweep(t, l)
	open3, err := l.ListDiscrepancies(ctx, false)
	must(t, err)
	reopened := findDiscrepancy(open3, a)
	if reopened == nil {
		t.Fatal("expected a new open discrepancy after resolving while the drift is still real")
	}
	if reopened.ID == found.ID {
		t.Fatal("expected a genuinely new discrepancy row, not the resolved one")
	}
}

func TestResolveDiscrepancy_AlreadyResolvedRejected(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	l := ledger.New(pool)

	a, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_resolve_twice_a"), "USD", "fiat", "A")
	must(t, err)
	b, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_resolve_twice_b"), "USD", "fiat", "B")
	must(t, err)
	_, err = l.Post(ctx, ledger.Transaction{
		IdempotencyKey: uuid.NewString(),
		TxnType:        "manual_adjustment",
		ReferenceType:  "test",
		ReferenceID:    uuid.New(),
		CreatedBy:      "test",
		Entries: []ledger.Entry{
			{AccountID: a, Direction: ledger.Debit, Amount: d("5"), AssetCode: "USD"},
			{AccountID: b, Direction: ledger.Credit, Amount: d("5"), AssetCode: "USD"},
		},
	})
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE ledger_balances SET balance = balance + 1 WHERE account_id = $1`, a)
	must(t, err)
	runReconcileSweep(t, l)

	open, err := l.ListDiscrepancies(ctx, false)
	must(t, err)
	found := findDiscrepancy(open, a)
	if found == nil {
		t.Fatal("expected an open discrepancy")
	}

	adminID := uuid.New()
	_, err = l.ResolveDiscrepancy(ctx, found.ID, adminID, "first resolution")
	must(t, err)

	_, err = l.ResolveDiscrepancy(ctx, found.ID, adminID, "second resolution")
	if !errors.Is(err, ledger.ErrDiscrepancyAlreadyResolved) {
		t.Fatalf("expected ErrDiscrepancyAlreadyResolved, got %v", err)
	}
}
