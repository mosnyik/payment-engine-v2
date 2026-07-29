// The persisted "best rate" pipeline: CurrentRateJob periodically computes
// GetBestQuote (system_rates ceiling vs. every enabled provider's quote,
// including CoinGecko via cmd/ratefetcher's provider_rates writes) and
// persists the result into current_rates. LockRate and the public
// GET /v2/rate/{fiatCurrency} endpoint both read that one persisted value
// rather than each re-deriving their own live selection.
package rate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
)

// ErrCurrentRateNotComputed means CurrentRateJob hasn't run for this
// currency yet — a fresh corridor's fiat currency between app start and
// the job's first tick, or a genuinely new currency with no corridor
// history at all. Self-resolving within one job interval; not a fallback
// trigger for a live recomputation (see ARCHITECTURE decision in the
// LockRate/currentrate design discussion — the persisted value is the
// single source of truth, not re-derived per caller).
var ErrCurrentRateNotComputed = errors.New("rate: current rate not computed yet for this fiat currency")

// CurrentRate is one persisted current_rates row.
type CurrentRate struct {
	FiatCurrency string
	Rate         decimal.Decimal
	Provider     string
	ComputedAt   time.Time
}

// ComputeAndPersistCurrentRate runs the existing best-quote selection
// (GetBestQuote) and upserts the result into current_rates — the one place
// "compute the best rate" happens, called by both CurrentRateJob's ticks
// and its startup warm-up run.
func (s *Store) ComputeAndPersistCurrentRate(ctx context.Context, fiatCurrency string) (*CurrentRate, error) {
	quote, err := s.GetBestQuote(ctx, fiatCurrency)
	if err != nil {
		return nil, err
	}

	cr := &CurrentRate{
		FiatCurrency: fiatCurrency,
		Rate:         quote.Rate,
		Provider:     quote.Provider,
		ComputedAt:   time.Now(),
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO current_rates (fiat_currency, rate, provider, computed_at)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (fiat_currency) DO UPDATE SET
		   rate = EXCLUDED.rate,
		   provider = EXCLUDED.provider,
		   computed_at = EXCLUDED.computed_at`,
		cr.FiatCurrency, cr.Rate, cr.Provider, cr.ComputedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("rate: persist current rate: %w", err)
	}
	return cr, nil
}

// GetCurrentRate returns the persisted current rate for fiatCurrency.
func (s *Store) GetCurrentRate(ctx context.Context, fiatCurrency string) (*CurrentRate, error) {
	cr := &CurrentRate{FiatCurrency: fiatCurrency}
	err := s.pool.QueryRow(ctx,
		`SELECT rate, provider, computed_at FROM current_rates WHERE fiat_currency = $1`,
		fiatCurrency,
	).Scan(&cr.Rate, &cr.Provider, &cr.ComputedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrCurrentRateNotComputed, fiatCurrency)
	}
	if err != nil {
		return nil, fmt.Errorf("rate: get current rate: %w", err)
	}
	return cr, nil
}

// CurrentRateJob is ticker-driven — same shape as FetchJob: started with
// `go job.Run(ctx)` from main.go.
type CurrentRateJob struct {
	store     *Store
	corridors *corridor.Store
	interval  time.Duration
}

func NewCurrentRateJob(store *Store, corridors *corridor.Store, interval time.Duration) *CurrentRateJob {
	return &CurrentRateJob{store: store, corridors: corridors, interval: interval}
}

// Run blocks until ctx is cancelled. Runs once immediately (so
// current_rates is warm before the first LockRate call, same convention
// FetchJob's own doc comment establishes), then every interval.
func (j *CurrentRateJob) Run(ctx context.Context) {
	j.runOnce(ctx)

	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.runOnce(ctx)
		}
	}
}

func (j *CurrentRateJob) runOnce(ctx context.Context) {
	currencies, err := j.corridors.ListActiveFiatCurrencies(ctx)
	if err != nil {
		log.Printf("rate: current rate job: list active fiat currencies: %v", err)
		return
	}
	for _, fiat := range currencies {
		if _, err := j.store.ComputeAndPersistCurrentRate(ctx, fiat); err != nil {
			log.Printf("rate: current rate job: compute %s: %v", fiat, err)
		}
	}
}
