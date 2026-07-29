package rate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/rate"
)

func TestComputeAndPersistCurrentRate_PersistsTheWinningQuote(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	fiat := uniqueFiat(t)
	if err := s.SetSystemRate(ctx, fiat, decimal.NewFromInt(1650), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}

	cr, err := s.ComputeAndPersistCurrentRate(ctx, fiat)
	if err != nil {
		t.Fatalf("compute and persist current rate: %v", err)
	}
	if cr.Provider != "system" {
		t.Fatalf("expected system to win (only enabled provider with data), got %q", cr.Provider)
	}
	if !cr.Rate.Equal(decimal.NewFromInt(1650)) {
		t.Fatalf("expected rate 1650, got %s", cr.Rate)
	}
	if cr.ComputedAt.IsZero() {
		t.Fatal("expected computed_at to be set")
	}

	got, err := s.GetCurrentRate(ctx, fiat)
	if err != nil {
		t.Fatalf("get current rate: %v", err)
	}
	if !got.Rate.Equal(cr.Rate) || got.Provider != cr.Provider {
		t.Fatalf("expected GetCurrentRate to round-trip what was persisted, got %+v", got)
	}
}

// TestComputeAndPersistCurrentRate_CoinGeckoIsPartOfTheSelection proves
// providers.go's readOnlyProvider is actually wired into GetBestQuote's
// provider list — a provider_rates row written the way cmd/ratefetcher
// writes it (UpsertProviderRate, provider=ProviderCoinGecko) competes on
// equal footing with the system ceiling, without CurrentRateJob (or
// anything else) ever making a live call to CoinGecko itself.
func TestComputeAndPersistCurrentRate_CoinGeckoIsPartOfTheSelection(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	fiat := uniqueFiat(t)
	// System ceiling higher than CoinGecko's quote — selectBest picks the
	// lower one (ARCHITECTURE.md §7: the system rate can only push the
	// locked rate down, external providers can win when they're lower).
	if err := s.SetSystemRate(ctx, fiat, decimal.NewFromInt(2000), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}
	if err := rate.UpsertProviderRate(ctx, pool, rate.Quote{
		Provider:  rate.ProviderCoinGecko,
		Rate:      decimal.NewFromInt(1600),
		FetchedAt: time.Now(),
	}, fiat); err != nil {
		t.Fatalf("upsert provider rate: %v", err)
	}

	cr, err := s.ComputeAndPersistCurrentRate(ctx, fiat)
	if err != nil {
		t.Fatalf("compute and persist current rate: %v", err)
	}
	if cr.Provider != rate.ProviderCoinGecko {
		t.Fatalf("expected coingecko (the lower quote) to win, got %q", cr.Provider)
	}
	if !cr.Rate.Equal(decimal.NewFromInt(1600)) {
		t.Fatalf("expected rate 1600, got %s", cr.Rate)
	}
}

func TestComputeAndPersistCurrentRate_UpsertsRatherThanDuplicates(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	fiat := uniqueFiat(t)
	if err := s.SetSystemRate(ctx, fiat, decimal.NewFromInt(1000), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}
	if _, err := s.ComputeAndPersistCurrentRate(ctx, fiat); err != nil {
		t.Fatalf("compute and persist current rate (first): %v", err)
	}

	if err := s.SetSystemRate(ctx, fiat, decimal.NewFromInt(1200), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("update system rate: %v", err)
	}
	if _, err := s.ComputeAndPersistCurrentRate(ctx, fiat); err != nil {
		t.Fatalf("compute and persist current rate (second): %v", err)
	}

	got, err := s.GetCurrentRate(ctx, fiat)
	if err != nil {
		t.Fatalf("get current rate: %v", err)
	}
	if !got.Rate.Equal(decimal.NewFromInt(1200)) {
		t.Fatalf("expected the second computation to overwrite the first, got %s", got.Rate)
	}
}

func TestGetCurrentRate_NotComputedYet(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	_, err := s.GetCurrentRate(ctx, "NEVER_COMPUTED_"+t.Name())
	if !errors.Is(err, rate.ErrCurrentRateNotComputed) {
		t.Fatalf("expected ErrCurrentRateNotComputed, got %v", err)
	}
}

func TestCurrentRateJob_ComputesForEveryActiveCorridorCurrency(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)
	corridorStore := corridor.New(pool)

	fiat := uniqueFiat(t)
	network := "TSTNET" + uuid.NewString()[:8]
	if _, err := corridorStore.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:   "USDT",
		CryptoNetwork: network,
		FiatCurrency:  fiat,
		Active:        true,
	}); err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}
	if err := s.SetSystemRate(ctx, fiat, decimal.NewFromInt(1300), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}

	// The job's runOnce iterates every active corridor currency in the
	// shared, long-lived dev database — this run's backlog only grows
	// across the lifetime of this test suite, so polling for this test's
	// own currency to show up is the robust check, not assuming the whole
	// sweep finishes within a fixed short window. A long tick interval:
	// only the "runs once immediately" behavior (same convention
	// FetchJob.Run's own doc comment establishes) matters here.
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := rate.NewCurrentRateJob(s, corridorStore, time.Hour)
	go job.Run(runCtx)

	deadline := time.Now().Add(15 * time.Second)
	var got *rate.CurrentRate
	var err error
	for time.Now().Before(deadline) {
		got, err = s.GetCurrentRate(context.Background(), fiat)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("expected the job's immediate run to have computed a current rate within 15s: %v", err)
	}
	if !got.Rate.Equal(decimal.NewFromInt(1300)) {
		t.Fatalf("expected rate 1300, got %s", got.Rate)
	}
}
