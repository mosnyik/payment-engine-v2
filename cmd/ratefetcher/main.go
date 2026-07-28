// Command ratefetcher is a standalone service, deployed as its own
// container independent of cmd/server (no shared process, no startup-order
// dependency on it — only on Postgres being reachable). It fetches the
// USD/<fiat> rate from CoinGecko (coingecko.go) on a fixed interval and
// persists it into provider_rates via rate.UpsertProviderRate — the same
// table/upsert path the main app's own in-process rate.FetchJob already
// uses for other providers (see internal/rate/rate.go's package doc
// comment). Deliberately does not reuse internal/platform/config.Config —
// that type requires TENANT_SECRET_ENCRYPTION_KEY and other main-app
// secrets this single-purpose service has no reason to need.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/rate"
)

type config struct {
	databaseURL     string
	coinGeckoAPIURL string
	coinGeckoAPIKey string
	fiatCurrency    string
	fetchInterval   time.Duration
}

func loadConfig() (config, error) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return config{}, fmt.Errorf("ratefetcher: DATABASE_URL is required")
	}

	fetchInterval := 20 * time.Minute
	if v := os.Getenv("COINGECKO_FETCH_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("ratefetcher: COINGECKO_FETCH_INTERVAL: invalid duration %q: %w", v, err)
		}
		fetchInterval = d
	}

	return config{
		databaseURL:     databaseURL,
		coinGeckoAPIURL: stringOrDefault("COINGECKO_API_URL", "https://api.coingecko.com/api/v3"),
		coinGeckoAPIKey: os.Getenv("COINGECKO_API_KEY"),
		fiatCurrency:    stringOrDefault("COINGECKO_FIAT_CURRENCY", "NGN"),
		fetchInterval:   fetchInterval,
	}, nil
}

func stringOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Open(ctx, cfg.databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Applies the same migrations/ directory cmd/server does — safe to run
	// concurrently with (or entirely without) that binary having started,
	// since golang-migrate takes a Postgres advisory lock. This is what
	// makes this service genuinely independent: it never needs cmd/server
	// to have run first, only Postgres to be reachable.
	if err := db.Migrate(cfg.databaseURL, "migrations"); err != nil {
		return err
	}
	log.Println("ratefetcher: db connected, migrations applied")

	client := newCoinGeckoClient(cfg.coinGeckoAPIURL, cfg.coinGeckoAPIKey)

	log.Printf("ratefetcher: fetching %s/USD from coingecko every %s", cfg.fiatCurrency, cfg.fetchInterval)
	runFetchLoop(ctx, pool, client, cfg.fiatCurrency, cfg.fetchInterval)
	log.Println("ratefetcher: shutting down")
	return nil
}

// runFetchLoop fetches immediately (so provider_rates is warm as soon as
// the container is up, same "run once before the first tick" shape
// rate.FetchJob.Run already establishes), then every interval. A single
// failed fetch is logged and never crashes the process — the next tick
// tries again.
func runFetchLoop(ctx context.Context, pool *db.Pool, client *coinGeckoClient, fiatCurrency string, interval time.Duration) {
	fetchOnce(ctx, pool, client, fiatCurrency)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fetchOnce(ctx, pool, client, fiatCurrency)
		}
	}
}

func fetchOnce(ctx context.Context, pool *db.Pool, client *coinGeckoClient, fiatCurrency string) {
	rateValue, err := client.FetchUSDFiatRate(ctx, fiatCurrency)
	if err != nil {
		log.Printf("ratefetcher: fetch %s rate: %v", fiatCurrency, err)
		return
	}

	fetchedAt := time.Now()
	err = rate.UpsertProviderRate(ctx, pool, rate.Quote{
		Provider:  rate.ProviderCoinGecko,
		Rate:      rateValue,
		FetchedAt: fetchedAt,
	}, fiatCurrency)
	if err != nil {
		log.Printf("ratefetcher: store %s rate: %v", fiatCurrency, err)
		return
	}
	log.Printf("ratefetcher: %s/USD = %s (coingecko)", fiatCurrency, rateValue)
}
