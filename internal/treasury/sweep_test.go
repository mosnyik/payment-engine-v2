package treasury

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

func TestIsStableAsset(t *testing.T) {
	cases := map[string]bool{
		"USDT":        true,
		"usdt":        true,
		"USDT-TRC20":  true,
		"BTC":         false,
		"ETH":         false,
		"BNB":         false,
		"TRX":         false,
	}
	for asset, want := range cases {
		if got := isStableAsset(asset); got != want {
			t.Fatalf("isStableAsset(%q) = %v, want %v", asset, got, want)
		}
	}
}

// insertTestReservation inserts a self-custody reservation row directly
// (below ReserveAddress) so sweep bookkeeping tests don't need a real
// wallet seed or provider wiring.
func insertTestReservation(t *testing.T, pool *db.Pool, corridorID uuid.UUID, chain wallet.Chain, address string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO treasury_address_reservations
		   (corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, 'self_custody_wallet', 'self_custody', 'ETH', $2, $3, 'reserved')
		 RETURNING id`,
		corridorID, string(chain), address,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
	return id
}

func insertConfirmedDeposit(t *testing.T, s *Store, reservationID uuid.UUID, txReference string, amount decimal.Decimal, confirmedAt time.Time) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO treasury_deposits (reservation_id, status, crypto_asset, amount, tx_reference, confirmed_at)
		 VALUES ($1, 'confirmed', 'USDT', $2, $3, $4)`,
		reservationID, amount, txReference, confirmedAt,
	)
	if err != nil {
		t.Fatalf("insert confirmed deposit: %v", err)
	}
}

func TestGetSweepableDeposits_SumsUnswept(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	_, corridorID := setupCorridor(t, pool)
	reservationID := insertTestReservation(t, pool, corridorID, wallet.Ethereum, "addr-"+uniqueAsset(t))
	ctx := context.Background()

	insertConfirmedDeposit(t, s, reservationID, "tx-1", decimal.NewFromInt(100), time.Now())
	insertConfirmedDeposit(t, s, reservationID, "tx-2", decimal.NewFromInt(50), time.Now())

	deposits, total, err := s.getSweepableDeposits(ctx, reservationID)
	if err != nil {
		t.Fatalf("get sweepable deposits: %v", err)
	}
	if len(deposits) != 2 {
		t.Fatalf("expected 2 deposits, got %d", len(deposits))
	}
	if !total.Equal(decimal.NewFromInt(150)) {
		t.Fatalf("expected total 150, got %s", total)
	}
}

func TestMarkDepositsSwept_ExcludesFromFutureQueries(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	_, corridorID := setupCorridor(t, pool)
	reservationID := insertTestReservation(t, pool, corridorID, wallet.Ethereum, "addr-"+uniqueAsset(t))
	ctx := context.Background()

	insertConfirmedDeposit(t, s, reservationID, "tx-1", decimal.NewFromInt(100), time.Now())
	deposits, _, err := s.getSweepableDeposits(ctx, reservationID)
	if err != nil {
		t.Fatalf("get sweepable deposits: %v", err)
	}
	ids := make([]uuid.UUID, len(deposits))
	for i, d := range deposits {
		ids[i] = d.ID
	}

	if err := s.markDepositsSwept(ctx, ids, uuid.New()); err != nil {
		t.Fatalf("mark swept: %v", err)
	}

	remaining, total, err := s.getSweepableDeposits(ctx, reservationID)
	if err != nil {
		t.Fatalf("get sweepable deposits after sweep: %v", err)
	}
	if len(remaining) != 0 || !total.Equal(decimal.Zero) {
		t.Fatalf("expected no sweepable deposits after marking swept, got %d totaling %s", len(remaining), total)
	}
}

func TestSweepDestination_UpsertRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	ctx := context.Background()
	chain := "ethereum_" + uniqueAsset(t)

	if err := s.SetSweepDestination(ctx, chain, "USDT", "0xcold1"); err != nil {
		t.Fatalf("set destination: %v", err)
	}
	got, err := s.getSweepDestination(ctx, chain, "USDT")
	if err != nil {
		t.Fatalf("get destination: %v", err)
	}
	if got != "0xcold1" {
		t.Fatalf("expected 0xcold1, got %s", got)
	}

	// Re-set — must update in place, not error.
	if err := s.SetSweepDestination(ctx, chain, "USDT", "0xcold2"); err != nil {
		t.Fatalf("re-set destination: %v", err)
	}
	got2, err := s.getSweepDestination(ctx, chain, "USDT")
	if err != nil {
		t.Fatalf("get destination after re-set: %v", err)
	}
	if got2 != "0xcold2" {
		t.Fatalf("expected updated 0xcold2, got %s", got2)
	}
}

func TestGetSweepDestination_NotConfigured(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	_, err := s.getSweepDestination(context.Background(), "ethereum", "DOES_NOT_EXIST_"+uniqueAsset(t))
	if !errors.Is(err, ErrNoSweepDestination) {
		t.Fatalf("expected ErrNoSweepDestination, got %v", err)
	}
}

func TestPersistAndUpdateSweep_RoundTrip(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	_, corridorID := setupCorridor(t, pool)
	reservationID := insertTestReservation(t, pool, corridorID, wallet.Ethereum, "addr-"+uniqueAsset(t))
	ctx := context.Background()

	sweepID, err := s.persistSweep(ctx, reservationID, "USDT", decimal.NewFromInt(500), "0xcold")
	if err != nil {
		t.Fatalf("persist sweep: %v", err)
	}

	if err := s.updateSweepStatus(ctx, sweepID, "broadcast", "0xtxhash"); err != nil {
		t.Fatalf("update sweep status: %v", err)
	}

	var status, txHash string
	err = pool.QueryRow(ctx, `SELECT status, tx_hash FROM treasury_sweeps WHERE id = $1`, sweepID).Scan(&status, &txHash)
	if err != nil {
		t.Fatalf("query sweep: %v", err)
	}
	if status != "broadcast" || txHash != "0xtxhash" {
		t.Fatalf("expected status=broadcast tx_hash=0xtxhash, got status=%s tx_hash=%s", status, txHash)
	}
}

func TestListStableSweepCandidates_ThresholdAndBackstop(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	_, corridorID := setupCorridor(t, pool)
	ctx := context.Background()

	// Above balance threshold, recent.
	aboveThreshold := insertTestReservation(t, pool, corridorID, wallet.Ethereum, "addr-above-"+uniqueAsset(t))
	insertConfirmedDeposit(t, s, aboveThreshold, "tx-above", decimal.NewFromInt(2000), time.Now())

	// Below threshold, but old enough to hit the time backstop.
	oldButSmall := insertTestReservation(t, pool, corridorID, wallet.Ethereum, "addr-old-"+uniqueAsset(t))
	insertConfirmedDeposit(t, s, oldButSmall, "tx-old", decimal.NewFromInt(10), time.Now().Add(-48*time.Hour))

	// Below threshold, recent — should not be a candidate.
	notYet := insertTestReservation(t, pool, corridorID, wallet.Ethereum, "addr-notyet-"+uniqueAsset(t))
	insertConfirmedDeposit(t, s, notYet, "tx-notyet", decimal.NewFromInt(10), time.Now())

	candidates, err := s.listStableSweepCandidates(ctx, decimal.NewFromInt(1000), 24*time.Hour)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}

	found := map[uuid.UUID]bool{}
	for _, id := range candidates {
		found[id] = true
	}
	if !found[aboveThreshold] {
		t.Fatalf("expected reservation above balance threshold to be a candidate")
	}
	if !found[oldButSmall] {
		t.Fatalf("expected old-but-small reservation to be a candidate via time backstop")
	}
	if found[notYet] {
		t.Fatalf("did not expect recent, below-threshold reservation to be a candidate")
	}
}

func TestExecuteSweep_NoSweepableDepositsIsNoOp(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	storeWithSeed(t, s)
	_, corridorID := setupCorridor(t, pool)
	reservationID := insertTestReservation(t, pool, corridorID, wallet.Ethereum, "addr-noop-"+uniqueAsset(t))

	sweeper := NewSweeper(s, SweepConfig{}, nil, nil, nil)
	if err := sweeper.ExecuteSweep(context.Background(), reservationID); err != nil {
		t.Fatalf("expected no-op (nil error) when nothing to sweep, got %v", err)
	}
}

func TestExecuteSweep_NoDestinationConfigured(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	storeWithSeed(t, s)
	_, corridorID := setupCorridor(t, pool)
	address := "addr-nodest-" + uniqueAsset(t)
	reservationID := insertTestReservation(t, pool, corridorID, wallet.Ethereum, address)
	insertConfirmedDeposit(t, s, reservationID, "tx-nodest", decimal.NewFromInt(100), time.Now())

	sweeper := NewSweeper(s, SweepConfig{}, nil, nil, nil)
	err := sweeper.ExecuteSweep(context.Background(), reservationID)
	if !errors.Is(err, ErrNoSweepDestination) {
		t.Fatalf("expected ErrNoSweepDestination, got %v", err)
	}
}
