package ledger_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")

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

func d(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

// uniqueType returns an account_type that won't collide with any other
// test run's data. These are true integration tests against a real,
// persistent database with no per-test transaction rollback or schema
// reset — GetOrCreateAccount's idempotent-lookup behavior means reusing a
// fixed account_type string across runs would silently accumulate balance
// from previous runs instead of starting fresh.
func uniqueType(base string) string {
	return base + "_" + uuid.NewString()[:8]
}

// TestWorkedExample reproduces the exact 100 USDT-TRC20 -> NGN flow from
// ARCHITECTURE.md §6, end to end, and checks every resulting balance.
func TestWorkedExample(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	l := ledger.New(pool)

	tenantID := uuid.New()
	sessionID := uuid.New()

	treasuryInTransit, err := l.GetOrCreateAccount(ctx, nil, uniqueType("treasury_in_transit"), "USDT", "crypto", "Treasury in-transit USDT")
	must(t, err)
	cryptoFxClearing, err := l.GetOrCreateAccount(ctx, nil, uniqueType("crypto_fx_clearing"), "USDT", "crypto", "Crypto FX clearing USDT")
	must(t, err)
	fiatFxClearing, err := l.GetOrCreateAccount(ctx, nil, uniqueType("fiat_fx_clearing"), "NGN", "fiat", "Fiat FX clearing NGN")
	must(t, err)
	tenantPayable, err := l.GetOrCreateAccount(ctx, &tenantID, uniqueType("tenant_payable"), "NGN", "fiat", "Tenant payable NGN")
	must(t, err)
	feeRevenue, err := l.GetOrCreateAccount(ctx, nil, uniqueType("fee_revenue"), "NGN", "fiat", "Fee revenue NGN")
	must(t, err)
	treasuryFiatOperating, err := l.GetOrCreateAccount(ctx, nil, uniqueType("treasury_fiat_operating"), "NGN", "fiat", "Treasury fiat operating NGN")
	must(t, err)
	treasuryCustody, err := l.GetOrCreateAccount(ctx, nil, uniqueType("treasury_custody"), "USDT", "crypto", "Treasury custody USDT")
	must(t, err)

	// Step 1: deposit confirmed (crypto leg)
	_, err = l.Post(ctx, ledger.Transaction{
		IdempotencyKey: "deposit_confirmed:" + sessionID.String(),
		TxnType:        "deposit_confirmed",
		ReferenceType:  "session",
		ReferenceID:    sessionID,
		CreatedBy:      "treasury",
		Entries: []ledger.Entry{
			{AccountID: treasuryInTransit, Direction: ledger.Debit, Amount: d("100"), AssetCode: "USDT"},
			{AccountID: cryptoFxClearing, Direction: ledger.Credit, Amount: d("100"), AssetCode: "USDT"},
		},
	})
	must(t, err)

	// Step 2: fiat liability recognized (fiat leg, linked by ReferenceID not by transaction)
	_, err = l.Post(ctx, ledger.Transaction{
		IdempotencyKey: "fx_conversion:" + sessionID.String(),
		TxnType:        "fx_conversion",
		ReferenceType:  "session",
		ReferenceID:    sessionID,
		CreatedBy:      "ledger",
		Entries: []ledger.Entry{
			{AccountID: fiatFxClearing, Direction: ledger.Debit, Amount: d("162000"), AssetCode: "NGN"},
			{AccountID: tenantPayable, Direction: ledger.Credit, Amount: d("160380"), AssetCode: "NGN"},
			{AccountID: feeRevenue, Direction: ledger.Credit, Amount: d("1620"), AssetCode: "NGN"},
		},
	})
	must(t, err)

	// Step 3: settlement dispatched (real-time, independent of sweep)
	_, err = l.Post(ctx, ledger.Transaction{
		IdempotencyKey: "settlement_payout:" + sessionID.String(),
		TxnType:        "settlement_payout",
		ReferenceType:  "session",
		ReferenceID:    sessionID,
		CreatedBy:      "settlement",
		Entries: []ledger.Entry{
			{AccountID: tenantPayable, Direction: ledger.Debit, Amount: d("160380"), AssetCode: "NGN"},
			{AccountID: treasuryFiatOperating, Direction: ledger.Credit, Amount: d("160380"), AssetCode: "NGN"},
		},
	})
	must(t, err)

	// Step 4: sweep (async, batched, independent of settlement)
	_, err = l.Post(ctx, ledger.Transaction{
		IdempotencyKey: "sweep:batch_1:" + sessionID.String(),
		TxnType:        "sweep",
		ReferenceType:  "session",
		ReferenceID:    sessionID,
		CreatedBy:      "treasury",
		Entries: []ledger.Entry{
			{AccountID: treasuryCustody, Direction: ledger.Debit, Amount: d("100"), AssetCode: "USDT"},
			{AccountID: treasuryInTransit, Direction: ledger.Credit, Amount: d("100"), AssetCode: "USDT"},
		},
	})
	must(t, err)

	// Final balances, per ARCHITECTURE.md's sign convention (debits - credits):
	checkBalance(t, ctx, l, treasuryInTransit, d("0"))       // +100 then -100
	checkBalance(t, ctx, l, treasuryCustody, d("100"))       // swept in
	checkBalance(t, ctx, l, cryptoFxClearing, d("-100"))     // credit only
	checkBalance(t, ctx, l, fiatFxClearing, d("162000"))     // debit only
	checkBalance(t, ctx, l, tenantPayable, d("0"))           // credited then debited to zero — liability settled
	checkBalance(t, ctx, l, feeRevenue, d("-1620"))          // credit only — revenue
	checkBalance(t, ctx, l, treasuryFiatOperating, d("-160380")) // paid out
}

func TestPost_RejectsUnbalancedEntries(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	l := ledger.New(pool)

	a, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_a"), "USD", "fiat", "A")
	must(t, err)
	b, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_b"), "USD", "fiat", "B")
	must(t, err)

	_, err = l.Post(ctx, ledger.Transaction{
		IdempotencyKey: uuid.NewString(),
		TxnType:        "manual_adjustment",
		ReferenceType:  "test",
		ReferenceID:    uuid.New(),
		CreatedBy:      "test",
		Entries: []ledger.Entry{
			{AccountID: a, Direction: ledger.Debit, Amount: d("100"), AssetCode: "USD"},
			{AccountID: b, Direction: ledger.Credit, Amount: d("99"), AssetCode: "USD"}, // deliberately off by 1
		},
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("expected ErrUnbalanced, got %v", err)
	}

	// Nothing should have been persisted — verify balances are untouched (zero).
	checkBalance(t, ctx, l, a, decimal.Zero)
	checkBalance(t, ctx, l, b, decimal.Zero)
}

func TestPost_UnbalancedAcrossDifferentAssetsStillRejected(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	l := ledger.New(pool)

	usd, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_multi_usd"), "USD", "fiat", "USD")
	must(t, err)
	eur, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_multi_eur"), "EUR", "fiat", "EUR")
	must(t, err)

	// Balanced in aggregate "value" if you squint, but each asset must
	// balance independently — USD entries alone don't net to zero.
	_, err = l.Post(ctx, ledger.Transaction{
		IdempotencyKey: uuid.NewString(),
		TxnType:        "manual_adjustment",
		ReferenceType:  "test",
		ReferenceID:    uuid.New(),
		CreatedBy:      "test",
		Entries: []ledger.Entry{
			{AccountID: usd, Direction: ledger.Debit, Amount: d("100"), AssetCode: "USD"},
			{AccountID: eur, Direction: ledger.Credit, Amount: d("100"), AssetCode: "EUR"},
		},
	})
	if !errors.Is(err, ledger.ErrUnbalanced) {
		t.Fatalf("expected ErrUnbalanced for cross-asset mismatch, got %v", err)
	}
}

func TestPost_DuplicateIdempotencyKeyRejected(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	l := ledger.New(pool)

	a, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_dup_a"), "USD", "fiat", "A")
	must(t, err)
	b, err := l.GetOrCreateAccount(ctx, nil, uniqueType("test_dup_b"), "USD", "fiat", "B")
	must(t, err)

	key := uuid.NewString()
	txn := ledger.Transaction{
		IdempotencyKey: key,
		TxnType:        "manual_adjustment",
		ReferenceType:  "test",
		ReferenceID:    uuid.New(),
		CreatedBy:      "test",
		Entries: []ledger.Entry{
			{AccountID: a, Direction: ledger.Debit, Amount: d("50"), AssetCode: "USD"},
			{AccountID: b, Direction: ledger.Credit, Amount: d("50"), AssetCode: "USD"},
		},
	}

	_, err = l.Post(ctx, txn)
	must(t, err)

	// Same key again — simulates a retried settlement dispatch. Must be
	// rejected, and must NOT double-apply the balance change.
	_, err = l.Post(ctx, txn)
	if !errors.Is(err, ledger.ErrAlreadyPosted) {
		t.Fatalf("expected ErrAlreadyPosted, got %v", err)
	}

	checkBalance(t, ctx, l, a, d("50"))
	checkBalance(t, ctx, l, b, d("-50"))
}

func TestGetOrCreateAccount_IdempotentForPlatformAccounts(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	l := ledger.New(pool)

	// Regression check for the NULLS NOT DISTINCT constraint: two
	// platform-level (tenant_id NULL) lookups with the same type+asset must
	// resolve to the SAME account, not silently create duplicates. The type
	// must stay fixed across both calls within this test (that's what's
	// being verified) but still be unique to this run so it doesn't collide
	// with a previous run's leftover data.
	accountType := uniqueType("test_platform_acct")
	id1, err := l.GetOrCreateAccount(ctx, nil, accountType, "USD", "fiat", "first name")
	must(t, err)
	id2, err := l.GetOrCreateAccount(ctx, nil, accountType, "USD", "fiat", "second name")
	must(t, err)

	if id1 != id2 {
		t.Fatalf("expected the same account for repeated platform-level lookups, got %s and %s", id1, id2)
	}
}

func TestPost_RequiresAtLeastOneEntry(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	l := ledger.New(pool)

	_, err := l.Post(ctx, ledger.Transaction{
		IdempotencyKey: uuid.NewString(),
		TxnType:        "manual_adjustment",
		ReferenceType:  "test",
		ReferenceID:    uuid.New(),
		CreatedBy:      "test",
		Entries:        nil,
	})
	if !errors.Is(err, ledger.ErrNoEntries) {
		t.Fatalf("expected ErrNoEntries, got %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func checkBalance(t *testing.T, ctx context.Context, l *ledger.Ledger, accountID uuid.UUID, want decimal.Decimal) {
	t.Helper()
	got, err := l.GetBalance(ctx, accountID)
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("balance mismatch for account %s: got %s, want %s", accountID, got, want)
	}
}
