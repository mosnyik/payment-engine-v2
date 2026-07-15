// Command adminctl provisions admin accounts. This is the only way to
// create one — there is deliberately no public HTTP admin-signup endpoint
// (see internal/platform/adminauth), since admin credentials gate wallet
// config, compliance decisions, and manual settlement.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func main() {
	email := flag.String("email", "", "admin email (required)")
	password := flag.String("password", "", "admin password (optional — a random one is generated and printed once if omitted)")
	flag.Parse()

	if *email == "" {
		fmt.Fprintln(os.Stderr, "usage: adminctl -email=you@example.com [-password=...]")
		os.Exit(1)
	}

	if err := run(*email, *password); err != nil {
		log.Fatal(err)
	}
}

func run(email, password string) error {
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

	// Safe to call even if the server has never run yet — this may be the
	// very first thing someone runs against a fresh database.
	if err := db.Migrate(cfg.DatabaseURL, "migrations"); err != nil {
		return err
	}

	generated := password == ""
	if generated {
		password, err = randomPassword()
		if err != nil {
			return err
		}
	}

	store := adminauth.New(pool, cfg.AdminSessionTTL)
	id, err := store.CreateAdmin(ctx, email, password)
	if err != nil {
		return fmt.Errorf("adminctl: create admin: %w", err)
	}

	fmt.Printf("Admin created: %s (id: %s)\n", email, id)
	if generated {
		fmt.Printf("Generated password (shown once — save it now): %s\n", password)
	}
	return nil
}

func randomPassword() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("adminctl: generate password: %w", err)
	}
	return hex.EncodeToString(b), nil
}
