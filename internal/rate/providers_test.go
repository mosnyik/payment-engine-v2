package rate_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/rate"
)

// TestUpsertProviderRate_RoundTripsThroughExistingReadPath proves the
// exported package-level UpsertProviderRate (cmd/ratefetcher's write path,
// since that binary has only a *db.Pool, never a full Store) lands in
// exactly the same place the in-process FetchJob already writes to —
// provider_rates — by reading it back through the real httpProvider.FetchRate
// path (Busha, enabled here) rather than a raw SQL assertion.
func TestUpsertProviderRate_RoundTripsThroughExistingReadPath(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	fiat := uniqueFiat(t)

	store := rate.New(pool, rate.Config{
		Busha: rate.ProviderConfig{Enabled: true},
	})

	fetchedAt := time.Now()
	err := rate.UpsertProviderRate(ctx, pool, rate.Quote{
		Provider:  "busha",
		Rate:      decimal.NewFromInt(1650),
		FetchedAt: fetchedAt,
	}, fiat)
	if err != nil {
		t.Fatalf("upsert provider rate: %v", err)
	}

	quotes := store.GetAllQuotes(ctx, fiat)
	if len(quotes) != 1 {
		t.Fatalf("expected exactly one quote (busha, no system rate set), got %d: %+v", len(quotes), quotes)
	}
	if quotes[0].Provider != "busha" {
		t.Fatalf("expected provider %q, got %q", "busha", quotes[0].Provider)
	}
	if !quotes[0].Rate.Equal(decimal.NewFromInt(1650)) {
		t.Fatalf("expected rate 1650, got %s", quotes[0].Rate)
	}

	// A second upsert for the same (provider, fiat_currency) must update in
	// place, not accumulate a duplicate row — UNIQUE (provider, fiat_currency)
	// plus the ON CONFLICT clause is what this is actually testing.
	err = rate.UpsertProviderRate(ctx, pool, rate.Quote{
		Provider:  "busha",
		Rate:      decimal.NewFromInt(1700),
		FetchedAt: time.Now(),
	}, fiat)
	if err != nil {
		t.Fatalf("second upsert provider rate: %v", err)
	}
	quotes = store.GetAllQuotes(ctx, fiat)
	if len(quotes) != 1 {
		t.Fatalf("expected still exactly one quote after a second upsert, got %d: %+v", len(quotes), quotes)
	}
	if !quotes[0].Rate.Equal(decimal.NewFromInt(1700)) {
		t.Fatalf("expected updated rate 1700, got %s", quotes[0].Rate)
	}
}
