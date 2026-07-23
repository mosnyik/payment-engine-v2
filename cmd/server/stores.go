package main

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/rate"
	"github.com/sirfi/payment-engine-v2/internal/session"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
	"github.com/sirfi/payment-engine-v2/internal/treasury"
)

// appStores is every module Store the running server needs, built exactly
// once and shared between the HTTP router and the background jobs
// (eventbus dispatcher, rate fetch job, session TTL sweep). Sharing matters
// specifically for treasuryStore/sessionStore/bus: an event published by
// one instance is invisible to a dispatcher running on another, so the
// HTTP-facing session-creation path and the started dispatcher must be the
// same objects, not independent copies.
type appStores struct {
	tenant     *tenant.Store
	compliance *compliance.Store
	admin      *adminauth.Store
	corridor   *corridor.Store
	rate       *rate.Store
	treasury   *treasury.Store
	session    *session.Store
	bus        *eventbus.Bus
}

func buildStores(cfg *config.Config, pool *db.Pool) (*appStores, error) {
	tenantStore, err := tenant.New(pool, cfg.TenantSecretEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("build stores: %w", err)
	}
	complianceStore := compliance.New(pool, compliance.NewRegistry())
	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	corridorStore := corridor.New(pool)

	rateStore := rate.New(pool, rate.Config{
		CoinMarketCapAPIKey: cfg.RateEngine.CoinMarketCapAPIKey,
		Busha:               rate.ProviderConfig(cfg.RateEngine.Busha),
		LiquidRamp:          rate.ProviderConfig(cfg.RateEngine.LiquidRamp),
		Anchor:              rate.ProviderConfig(cfg.RateEngine.Anchor),
	})

	stableThreshold, err := decimal.NewFromString(cfg.Treasury.Sweep.StableBalanceThreshold)
	if err != nil {
		return nil, fmt.Errorf("build stores: invalid SWEEP_STABLE_BALANCE_THRESHOLD: %w", err)
	}
	treasuryStore := treasury.New(pool, corridorStore, treasury.Config{
		Busha:    treasury.CollectionProviderConfig(cfg.Treasury.Busha),
		Bitcoin:  treasury.ChainConfig(cfg.Treasury.Bitcoin),
		Ethereum: treasury.ChainConfig(cfg.Treasury.Ethereum),
		BSC:      treasury.ChainConfig(cfg.Treasury.BSC),
		Tron:     treasury.ChainConfig(cfg.Treasury.Tron),
		Watcher:  treasury.WatcherConfig(cfg.Treasury.Watcher),
		Sweep: treasury.SweepConfig{
			StableBalanceThreshold: stableThreshold,
			StableTimeBackstop:     cfg.Treasury.Sweep.StableTimeBackstop,
		},
		TenantWebhookTimeout:    cfg.Treasury.TenantWebhookTimeout,
		TenantWebhookMaxRetries: cfg.Treasury.TenantWebhookMaxRetries,
	})

	// The eventbus dispatcher itself is started from main.go (needs a
	// long-lived ctx); every subscriber must register before that happens
	// (see eventbus.Subscribe's doc comment), so wiring publishers/
	// subscribers happens here, at construction time.
	bus := eventbus.New(pool, cfg.EventbusBatchSize)
	treasuryStore.SetEventBus(bus)

	sessionStore := session.New(pool, corridorStore, complianceStore, rateStore, treasuryStore, tenantStore, bus)
	sessionStore.RegisterEventHandlers()

	return &appStores{
		tenant:     tenantStore,
		compliance: complianceStore,
		admin:      adminStore,
		corridor:   corridorStore,
		rate:       rateStore,
		treasury:   treasuryStore,
		session:    sessionStore,
		bus:        bus,
	}, nil
}
