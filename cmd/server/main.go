// Command server is the single binary that runs the whole modular monolith.
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/sirfi/payment-engine-v2/internal/notification"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/rate"
	"github.com/sirfi/payment-engine-v2/internal/session"
	"github.com/sirfi/payment-engine-v2/internal/settlement"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// The single point secrets/config are loaded and validated — nothing
	// else in the codebase reads os.Getenv directly. Swapping .env for a
	// real secrets manager later means changing this one call.
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}

	ctx := context.Background()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(cfg.DatabaseURL, "migrations"); err != nil {
		return err
	}

	log.Println("payment-engine-v2: db connected, migrations applied")

	// Builds every module Store once, including wiring treasury's
	// SetEventBus and session's RegisterEventHandlers — both must happen
	// before bus.Run starts below (see appStores' doc comment).
	stores, err := buildStores(cfg, pool)
	if err != nil {
		return err
	}

	router, err := buildRouter(cfg, stores)
	if err != nil {
		return err
	}

	// The eventbus dispatcher: first real Publish call sites are
	// treasury's deposit-state transitions and session's CreateSession/
	// event handlers (Phase 5).
	go stores.bus.Run(ctx, cfg.EventbusPollInterval)

	fetchJob := rate.NewFetchJob(stores.rate, stores.corridor, cfg.RateEngine.FetchInterval)
	go fetchJob.Run(ctx)

	ttlJob := session.NewTTLJob(stores.session, cfg.Session.TTLCheckInterval)
	go ttlJob.Run(ctx)

	// settlement's ledger-claim-then-dispatch pipeline (Phase 6) — fired by
	// session.deposit_confirmed, picked up here rather than inline in that
	// event's handler since ledger.Post can never run inside an
	// eventbus.Handler's transaction (see internal/settlement's package doc).
	dispatchWorker := settlement.NewDispatchWorker(stores.settlement, cfg.Settlement.DispatchPollInterval)
	go dispatchWorker.Run(ctx)

	timeoutJob := settlement.NewTimeoutJob(stores.settlement, cfg.Settlement.TimeoutCheckPollInterval)
	go timeoutJob.Run(ctx)

	// notification's delivery sender (Phase 7) — same "eventbus handler
	// claims a local row, this ticker-driven worker does the real network
	// call" split settlement's DispatchWorker already establishes.
	notificationDispatchWorker := notification.NewDispatchWorker(stores.notification, cfg.Notification.DispatchPollInterval)
	go notificationDispatchWorker.Run(ctx)

	log.Printf("payment-engine-v2: listening on %s", cfg.HTTPAddr)
	return http.ListenAndServe(cfg.HTTPAddr, router)
}
