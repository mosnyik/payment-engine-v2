// Command server is the single binary that runs the whole modular monolith.
package main

import (
	"context"
	"log"
	"os"

	"github.com/joho/godotenv"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	_ = godotenv.Load() // .env is optional — real deployments set env vars directly

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	ctx := context.Background()

	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(databaseURL, "migrations"); err != nil {
		return err
	}

	log.Println("payment-engine-v2: db connected, migrations applied")
	return nil
}
