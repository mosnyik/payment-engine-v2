// Command server is the single binary that runs the whole modular monolith.
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
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

	router, err := buildRouter(cfg, pool)
	if err != nil {
		return err
	}

	// eventbus's dispatcher isn't started here yet — no module publishes
	// real domain events to the outbox to dispatch. It'll start alongside
	// the first module that does (Phase 5 onward).

	log.Printf("payment-engine-v2: listening on %s", cfg.HTTPAddr)
	return http.ListenAndServe(cfg.HTTPAddr, router)
}
