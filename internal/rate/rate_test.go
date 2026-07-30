package rate_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/rate"
)

func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return id
}

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

// uniqueFiat keeps each test's fiat currency from colliding with data left
// over from a previous run against the same live database.
func uniqueFiat(t *testing.T) string {
	t.Helper()
	return "TST_" + t.Name()
}

// newStoreWithNoExternalProviders builds a Store where only the always-on
// system provider is enabled — none of Busha/LiquidRamp/Anchor have API
// credentials configured, so tests never make a real outbound HTTP call.
func newStoreWithNoExternalProviders(pool *db.Pool) *rate.Store {
	return rate.New(pool, rate.Config{})
}

func TestSetAndGetSystemRate(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	fiat := uniqueFiat(t)
	err := s.SetSystemRate(ctx, fiat,
		decimal.NewFromInt(1650), decimal.NewFromInt(1600), decimal.NewFromInt(20))
	if err != nil {
		t.Fatalf("set system rate: %v", err)
	}

	got, err := s.GetSystemRate(ctx, fiat)
	if err != nil {
		t.Fatalf("get system rate: %v", err)
	}
	if !got.CurrentRate.Equal(decimal.NewFromInt(1650)) {
		t.Fatalf("expected current rate 1650, got %s", got.CurrentRate)
	}
	if !got.MerchantRate.Equal(decimal.NewFromInt(1600)) {
		t.Fatalf("expected merchant rate 1600, got %s", got.MerchantRate)
	}

	// Re-set with different values — must update in place, not error or
	// create a second row.
	if err := s.SetSystemRate(ctx, fiat, decimal.NewFromInt(1700), decimal.NewFromInt(1650), decimal.NewFromInt(25)); err != nil {
		t.Fatalf("re-set system rate: %v", err)
	}
	got2, err := s.GetSystemRate(ctx, fiat)
	if err != nil {
		t.Fatalf("get system rate after re-set: %v", err)
	}
	if !got2.CurrentRate.Equal(decimal.NewFromInt(1700)) {
		t.Fatalf("expected updated current rate 1700, got %s", got2.CurrentRate)
	}
}

func TestGetSystemRate_NotSet(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	_, err := s.GetSystemRate(ctx, "DOES_NOT_EXIST_"+t.Name())
	if !errors.Is(err, rate.ErrSystemRateNotSet) {
		t.Fatalf("expected ErrSystemRateNotSet, got %v", err)
	}
}

func TestGetBestQuote_SystemOnly(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	fiat := uniqueFiat(t)
	if err := s.SetSystemRate(ctx, fiat, decimal.NewFromInt(1500), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}

	quote, err := s.GetBestQuote(ctx, fiat)
	if err != nil {
		t.Fatalf("get best quote: %v", err)
	}
	if quote.Provider != "system" {
		t.Fatalf("expected system provider to win when it's the only enabled one, got %q", quote.Provider)
	}
	if !quote.Rate.Equal(decimal.NewFromInt(1500)) {
		t.Fatalf("expected rate 1500, got %s", quote.Rate)
	}
}

func TestGetBestQuote_NoQuotesAvailable(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	_, err := s.GetBestQuote(ctx, "NO_SYSTEM_RATE_SET_"+t.Name())
	if !errors.Is(err, rate.ErrNoQuotesAvailable) {
		t.Fatalf("expected ErrNoQuotesAvailable, got %v", err)
	}
}

func TestGetAssetPrice_USDTIsAlwaysOne(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	price, err := s.GetAssetPrice(ctx, "USDT")
	if err != nil {
		t.Fatalf("get asset price: %v", err)
	}
	if !price.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("expected USDT price 1, got %s", price)
	}
}

func TestLockRate(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	fiat := uniqueFiat(t)
	if err := s.SetSystemRate(ctx, fiat, decimal.NewFromInt(1000), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}
	// LockRate reads the persisted current_rates snapshot (CurrentRateJob's
	// job), not GetBestQuote live — seed it the same way the job would.
	if _, err := s.ComputeAndPersistCurrentRate(ctx, fiat); err != nil {
		t.Fatalf("compute and persist current rate: %v", err)
	}

	lock, err := s.LockRate(ctx, "USDT", fiat, rate.DefaultLockTTL)
	if err != nil {
		t.Fatalf("lock rate: %v", err)
	}

	// 1% slippage buffer subtracted from the selected quote — see
	// ARCHITECTURE.md §7.
	wantRate := decimal.NewFromInt(990)
	if !lock.Rate.Equal(wantRate) {
		t.Fatalf("expected slippage-adjusted rate %s, got %s", wantRate, lock.Rate)
	}
	if !lock.AssetPriceUSD.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("expected USDT asset price 1, got %s", lock.AssetPriceUSD)
	}
	if lock.Provider != "system" {
		t.Fatalf("expected system provider, got %q", lock.Provider)
	}
	if lock.IsExpired() {
		t.Fatalf("freshly-created lock must not be expired")
	}
	if lock.ID.String() == "" {
		t.Fatalf("expected a persisted lock id")
	}

	// A lock persisted with a fiat amount converts back and forth
	// consistently.
	fiatAmount := decimal.NewFromInt(9900)
	cryptoAmount := lock.FiatToCrypto(fiatAmount)
	if !cryptoAmount.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("expected 9900 %s to convert to 10 USDT, got %s", fiat, cryptoAmount)
	}
	backToFiat := lock.CryptoToFiat(cryptoAmount)
	if !backToFiat.Equal(fiatAmount) {
		t.Fatalf("expected round-trip conversion to return to %s, got %s", fiatAmount, backToFiat)
	}

	got, err := s.GetLock(ctx, lock.ID)
	if err != nil {
		t.Fatalf("get lock: %v", err)
	}
	if !got.Rate.Equal(lock.Rate) || got.FiatCurrency != fiat || got.CryptoAsset != "USDT" {
		t.Fatalf("expected persisted lock to round-trip, got %+v", got)
	}
}

func TestLockRate_TTLExpiry(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	fiat := uniqueFiat(t)
	if err := s.SetSystemRate(ctx, fiat, decimal.NewFromInt(1000), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}
	if _, err := s.ComputeAndPersistCurrentRate(ctx, fiat); err != nil {
		t.Fatalf("compute and persist current rate: %v", err)
	}

	lock, err := s.LockRate(ctx, "USDT", fiat, -1*time.Second)
	if err != nil {
		t.Fatalf("lock rate: %v", err)
	}
	if !lock.IsExpired() {
		t.Fatalf("expected a lock created with a negative ttl to already be expired")
	}
}

func TestGetLock_NotFound(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStoreWithNoExternalProviders(pool)

	_, err := s.GetLock(ctx, mustParseUUID(t, "00000000-0000-0000-0000-000000000000"))
	if !errors.Is(err, rate.ErrLockNotFound) {
		t.Fatalf("expected ErrLockNotFound, got %v", err)
	}
}
