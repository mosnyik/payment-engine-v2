package rate

import (
	"context"
	"log"
	"time"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
)

// FetchJob keeps provider_rates warm so FetchRate/LockRate never make a
// live external call in-band. Polls the fiat-currency list from corridor
// (ARCHITECTURE.md §7 adaptation 2) instead of a hardcoded list — adding a
// fiat currency to a corridor is enough to bring it into scope here, no
// redeploy required.
type FetchJob struct {
	store     *Store
	corridors *corridor.Store
	interval  time.Duration
}

func NewFetchJob(store *Store, corridors *corridor.Store, interval time.Duration) *FetchJob {
	return &FetchJob{store: store, corridors: corridors, interval: interval}
}

// Run starts the poll loop. Blocks until ctx is cancelled. If no external
// provider is enabled, it logs and returns immediately rather than ticking
// pointlessly — matches v1's behavior.
func (j *FetchJob) Run(ctx context.Context) {
	if !j.hasEnabledFetchers() {
		log.Println("rate: fetch job: no external rate providers enabled — not starting")
		return
	}

	// Run once immediately so caches are warm before the first LockRate call.
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

func (j *FetchJob) hasEnabledFetchers() bool {
	for _, p := range j.store.providers {
		if _, ok := p.(LiveFetcher); ok && p.IsEnabled() {
			return true
		}
	}
	return false
}

func (j *FetchJob) runOnce(ctx context.Context) {
	currencies, err := j.corridors.ListActiveFiatCurrencies(ctx)
	if err != nil {
		log.Printf("rate: fetch job: list active fiat currencies: %v", err)
		return
	}
	if len(currencies) == 0 {
		return
	}

	for _, p := range j.store.providers {
		fetcher, ok := p.(LiveFetcher)
		if !ok || !p.IsEnabled() {
			continue
		}
		for _, fiat := range currencies {
			quote, err := fetcher.FetchLiveRate(ctx, fiat)
			if err != nil {
				log.Printf("rate: fetch job: provider %q (%s): %v", p.Name(), fiat, err)
				continue
			}
			if err := j.store.upsertProviderRate(ctx, quote, fiat); err != nil {
				log.Printf("rate: fetch job: store %q (%s): %v", p.Name(), fiat, err)
			}
		}
	}
}
