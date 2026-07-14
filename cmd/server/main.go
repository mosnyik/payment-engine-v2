// Command server is the single binary that runs the whole modular monolith.
package main

import (
	"context"
	"log"

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

	// gateway, adminauth, and eventbus are all built and config-driven
	// (cfg.HMACClockSkew, cfg.AdminSessionTTL, cfg.EventbusBatchSize) but
	// not wired into a running server yet — that needs a CredentialLookup
	// implementation, which doesn't exist until the tenant module (Phase 2)
	// is built. They'll be constructed and started here as those land.

	return nil
}
