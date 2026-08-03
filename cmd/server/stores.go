package main

import (
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/notification"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/rate"
	"github.com/sirfi/payment-engine-v2/internal/session"
	"github.com/sirfi/payment-engine-v2/internal/settlement"
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
	tenant       *tenant.Store
	compliance   *compliance.Store
	admin        *adminauth.Store
	corridor     *corridor.Store
	rate         *rate.Store
	treasury     *treasury.Store
	session      *session.Store
	ledger       *ledger.Ledger
	settlement   *settlement.Store
	notification *notification.Store
	bus          *eventbus.Bus
}

func buildStores(cfg *config.Config, pool *db.Pool) (*appStores, error) {
	tenantStore, err := tenant.New(pool, cfg.TenantSecretEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("build stores: %w", err)
	}
	complianceRegistry := compliance.NewRegistry()
	if cfg.SandboxMode {
		complianceRegistry.Register(compliance.SandboxProvider{})
	}
	complianceStore := compliance.New(pool, complianceRegistry)
	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	corridorStore := corridor.New(pool)

	rateStore := rate.New(pool, rate.Config{
		CoinMarketCapAPIKey: cfg.RateEngine.CoinMarketCapAPIKey,
		Busha:               rate.ProviderConfig(cfg.RateEngine.Busha),
		LiquidRamp:          rate.ProviderConfig(cfg.RateEngine.LiquidRamp),
		Anchor:              rate.ProviderConfig(cfg.RateEngine.Anchor),
	})

	// A hand-built config.Config (e.g. a test fixture that doesn't go
	// through config.Load, which always defaults this to "1000") may leave
	// this as the zero-value empty string — treated as 0, not a startup
	// failure, since no test exercises real sweep-threshold behavior
	// through this path.
	stableThresholdStr := cfg.Treasury.Sweep.StableBalanceThreshold
	if stableThresholdStr == "" {
		stableThresholdStr = "0"
	}
	stableThreshold, err := decimal.NewFromString(stableThresholdStr)
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
		SandboxMode:             cfg.SandboxMode,
	})

	// The eventbus dispatcher itself is started from main.go (needs a
	// long-lived ctx); every subscriber must register before that happens
	// (see eventbus.Subscribe's doc comment), so wiring publishers/
	// subscribers happens here, at construction time.
	bus := eventbus.New(pool, cfg.EventbusBatchSize)
	treasuryStore.SetEventBus(bus)
	complianceStore.SetEventBus(bus)

	sessionStore := session.New(pool, corridorStore, complianceStore, rateStore, treasuryStore, tenantStore, bus)
	sessionStore.RegisterEventHandlers()

	ledgerStore := ledger.New(pool)
	ledgerStore.SetEventBus(bus)
	settlementStore := settlement.New(pool, ledgerStore, corridorStore, sessionStore, treasuryStore, rateStore, tenantStore, settlement.Config{
		CNGN:        settlement.SettlementProviderConfig(cfg.Settlement.CNGN),
		Flutterwave: settlement.SettlementProviderConfig(cfg.Settlement.Flutterwave),
		Paystack:    settlement.SettlementProviderConfig(cfg.Settlement.Paystack),
		Monnify:     settlement.SettlementProviderConfig(cfg.Settlement.Monnify),
		HydrogenPay: settlement.SettlementProviderConfig(cfg.Settlement.HydrogenPay),
		SandboxMode: cfg.SandboxMode,
	})
	settlementStore.SetEventBus(bus)
	settlementStore.RegisterEventHandlers()

	notificationStore := notification.New(pool, tenantStore, notification.Config{
		OpsAlertEmail: cfg.Notification.OpsAlertEmail,
		Email:         notification.EmailProviderConfig(cfg.Notification.Email),
	})
	notificationStore.SetEventBus(bus)
	notificationStore.RegisterEventHandlers()

	return &appStores{
		tenant:       tenantStore,
		compliance:   complianceStore,
		admin:        adminStore,
		corridor:     corridorStore,
		rate:         rateStore,
		treasury:     treasuryStore,
		session:      sessionStore,
		ledger:       ledgerStore,
		settlement:   settlementStore,
		notification: notificationStore,
		bus:          bus,
	}, nil
}
