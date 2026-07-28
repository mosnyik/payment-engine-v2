// Package rate is the FX + asset-price engine: provider adapters feed a
// background job that keeps provider_rates warm, and LockRate() combines
// the selected fiat/USD quote with a crypto/USD asset price into an
// audit-able, persisted lock. Ported from v1 (see ARCHITECTURE.md §7) with
// three adaptations: Postgres instead of MySQL, a corridor-driven fiat
// currency list instead of a hardcoded one (see fetchjob.go), and a
// persisted rate_locks table instead of an in-memory-only lock.
package rate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

var (
	ErrNoQuotesAvailable = errors.New("rate: no provider quotes available")
	ErrSystemRateNotSet  = errors.New("rate: system rate not set for this fiat currency")
	ErrLockNotFound      = errors.New("rate: lock not found")
)

// slippageBuffer is subtracted from the selected quote before locking (see
// ARCHITECTURE.md §7). Not an operational config knob like the tunables in
// platform/config — it's a fixed design decision, not something ops vary
// per environment.
var slippageBuffer = decimal.NewFromFloat(0.01)

// DefaultLockTTL matches v1's default. Callers (Phase 5's session module)
// may pass a different ttl to LockRate per corridor/session policy.
const DefaultLockTTL = 30 * time.Minute

// SystemRate is the ops-configured ceiling rate for one fiat currency.
type SystemRate struct {
	FiatCurrency string
	CurrentRate  decimal.Decimal
	MerchantRate decimal.Decimal
	ProfitRate   decimal.Decimal
	UpdatedAt    time.Time
}

// Lock is a persisted, audit-able record of one locked exchange rate.
type Lock struct {
	ID            uuid.UUID
	CryptoAsset   string
	FiatCurrency  string
	Rate          decimal.Decimal // fiat per USD, post-slippage
	AssetPriceUSD decimal.Decimal // crypto per USD
	Provider      string          // which provider's quote was selected
	LockedAt      time.Time
	ExpiresAt     time.Time
}

// IsExpired reports whether the lock's TTL has elapsed.
func (l Lock) IsExpired() bool {
	return time.Now().After(l.ExpiresAt)
}

// FiatToCrypto converts a fiat amount to crypto at this lock's rate.
func (l Lock) FiatToCrypto(fiatAmount decimal.Decimal) decimal.Decimal {
	usdAmount := fiatAmount.Div(l.Rate)
	return usdAmount.Div(l.AssetPriceUSD)
}

// CryptoToFiat converts a crypto amount to fiat at this lock's rate.
func (l Lock) CryptoToFiat(cryptoAmount decimal.Decimal) decimal.Decimal {
	usdAmount := cryptoAmount.Mul(l.AssetPriceUSD)
	return usdAmount.Mul(l.Rate)
}

// ProviderConfig configures one external HTTP rate-provider adapter.
type ProviderConfig struct {
	Enabled bool
	APIURL  string
	APIKey  string
}

// Config is what main.go builds from *config.Config to construct a Store.
// Kept separate from config.Config so this package doesn't import
// platform/config — the same convention tenant/corridor/compliance follow,
// taking already-extracted values rather than the whole config object.
type Config struct {
	CoinMarketCapAPIKey string
	Busha               ProviderConfig
	LiquidRamp          ProviderConfig
	Anchor              ProviderConfig
}

type Store struct {
	pool        *db.Pool
	providers   []Provider
	assetPrices *assetPriceCache
	cmcAPIKey   string
}

func New(pool *db.Pool, cfg Config) *Store {
	return &Store{
		pool:        pool,
		assetPrices: newAssetPriceCache(),
		cmcAPIKey:   cfg.CoinMarketCapAPIKey,
		providers: []Provider{
			&systemProvider{pool: pool},
			newBushaProvider(pool, cfg.Busha),
			newLiquidRampProvider(pool, cfg.LiquidRamp),
			newAnchorProvider(pool, cfg.Anchor),
		},
	}
}

// GetSystemRate returns the ops-configured ceiling rate for fiatCurrency.
func (s *Store) GetSystemRate(ctx context.Context, fiatCurrency string) (*SystemRate, error) {
	var r SystemRate
	r.FiatCurrency = fiatCurrency
	err := s.pool.QueryRow(ctx,
		`SELECT current_rate, merchant_rate, profit_rate, updated_at
		 FROM system_rates WHERE fiat_currency = $1`,
		fiatCurrency,
	).Scan(&r.CurrentRate, &r.MerchantRate, &r.ProfitRate, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrSystemRateNotSet, fiatCurrency)
	}
	if err != nil {
		return nil, fmt.Errorf("rate: get system rate: %w", err)
	}
	return &r, nil
}

// SetSystemRate upserts the ops-configured ceiling rate for fiatCurrency —
// config-driven per ARCHITECTURE.md §7 adaptation 2, not a hardcoded
// single-currency row like v1.
func (s *Store) SetSystemRate(ctx context.Context, fiatCurrency string, currentRate, merchantRate, profitRate decimal.Decimal) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO system_rates (fiat_currency, current_rate, merchant_rate, profit_rate)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (fiat_currency) DO UPDATE SET
		   current_rate = EXCLUDED.current_rate,
		   merchant_rate = EXCLUDED.merchant_rate,
		   profit_rate = EXCLUDED.profit_rate,
		   updated_at = now()`,
		fiatCurrency, currentRate, merchantRate, profitRate,
	)
	if err != nil {
		return fmt.Errorf("rate: set system rate: %w", err)
	}
	return nil
}

// LockRate combines the selected fiat/USD quote with the crypto asset's
// USD price into a persisted lock (see ARCHITECTURE.md §7). ttl is the
// caller's responsibility — Phase 5's session module ties it to
// session/corridor policy; DefaultLockTTL exists only as a convenience.
func (s *Store) LockRate(ctx context.Context, cryptoAsset, fiatCurrency string, ttl time.Duration) (*Lock, error) {
	quote, err := s.GetBestQuote(ctx, fiatCurrency)
	if err != nil {
		return nil, err
	}
	assetPrice, err := s.GetAssetPrice(ctx, cryptoAsset)
	if err != nil {
		return nil, err
	}

	adjustment := slippageBuffer.Mul(quote.Rate)
	adjustedRate := quote.Rate.Sub(adjustment)

	now := time.Now()
	lock := &Lock{
		CryptoAsset:   cryptoAsset,
		FiatCurrency:  fiatCurrency,
		Rate:          adjustedRate,
		AssetPriceUSD: assetPrice,
		Provider:      quote.Provider,
		LockedAt:      now,
		ExpiresAt:     now.Add(ttl),
	}

	err = s.pool.QueryRow(ctx,
		`INSERT INTO rate_locks
		   (crypto_asset, fiat_currency, rate, asset_price_usd, provider, locked_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id`,
		lock.CryptoAsset, lock.FiatCurrency, lock.Rate, lock.AssetPriceUSD,
		lock.Provider, lock.LockedAt, lock.ExpiresAt,
	).Scan(&lock.ID)
	if err != nil {
		return nil, fmt.Errorf("rate: persist lock: %w", err)
	}
	return lock, nil
}

// GetLock looks up a previously persisted lock by id.
func (s *Store) GetLock(ctx context.Context, id uuid.UUID) (*Lock, error) {
	var l Lock
	l.ID = id
	err := s.pool.QueryRow(ctx,
		`SELECT crypto_asset, fiat_currency, rate, asset_price_usd, provider, locked_at, expires_at
		 FROM rate_locks WHERE id = $1`,
		id,
	).Scan(&l.CryptoAsset, &l.FiatCurrency, &l.Rate, &l.AssetPriceUSD, &l.Provider, &l.LockedAt, &l.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLockNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rate: get lock: %w", err)
	}
	return &l, nil
}

func (s *Store) upsertProviderRate(ctx context.Context, quote Quote, fiatCurrency string) error {
	return UpsertProviderRate(ctx, s.pool, quote, fiatCurrency)
}

// UpsertProviderRate records one provider's fiat-per-USD quote into
// provider_rates — the shared write path for both the in-process FetchJob
// (via Store.upsertProviderRate above) and any standalone rate-fetcher
// service (e.g. cmd/ratefetcher) that only has a *db.Pool, not a full
// Store — Store also wires up every other provider/CoinMarketCap, which a
// single-purpose fetcher has no business touching.
func UpsertProviderRate(ctx context.Context, pool *db.Pool, quote Quote, fiatCurrency string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO provider_rates (provider, fiat_currency, rate, fetched_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (provider, fiat_currency) DO UPDATE SET
		   rate = EXCLUDED.rate,
		   fetched_at = EXCLUDED.fetched_at,
		   updated_at = now()`,
		quote.Provider, fiatCurrency, quote.Rate, quote.FetchedAt,
	)
	if err != nil {
		return fmt.Errorf("rate: upsert provider rate: %w", err)
	}
	return nil
}
