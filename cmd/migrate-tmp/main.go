package main

import (
	"log"

	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func main() {
	cfg, err := config.LoadEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := db.Migrate(cfg.DatabaseURL, "migrations"); err != nil {
		log.Fatal(err)
	}
	log.Println("migrations applied")
}
