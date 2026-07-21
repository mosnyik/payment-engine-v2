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

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/treasury"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

func main() {
	email := flag.String("email", "", "admin email (required, unless -init-wallet)")
	password := flag.String("password", "", "admin password (optional — a random one is generated and printed once if omitted)")
	initWallet := flag.Bool("init-wallet", false, "provision the self-custody HD wallet seed (one-time; requires HD_WALLET_SEED_ENCRYPTION_KEY)")
	flag.Parse()

	if *initWallet {
		if err := runInitWallet(); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *email == "" {
		fmt.Fprintln(os.Stderr, "usage: adminctl -email=you@example.com [-password=...]\n   or: adminctl -init-wallet")
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

// runInitWallet provisions the self-custody HD wallet seed: generates a
// new BIP39 mnemonic, encrypts it, and persists the ciphertext as the
// singleton hd_wallet_seed row. One-time — a second run fails clearly
// (ErrHDWalletAlreadyInitialized) rather than silently doing nothing,
// since silently no-op-ing a wallet-provisioning command would be a bad
// surprise. The mnemonic itself is printed exactly once and never
// logged again — same discipline as CreateAdmin's generated password.
func runInitWallet() error {
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	if len(cfg.Treasury.HDWalletSeedEncryptionKey) == 0 {
		return fmt.Errorf("adminctl: HD_WALLET_SEED_ENCRYPTION_KEY must be set (32 bytes hex) before running -init-wallet")
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

	mnemonic, err := wallet.GenerateMnemonic()
	if err != nil {
		return fmt.Errorf("adminctl: generate mnemonic: %w", err)
	}

	corridorStore := corridor.New(pool)
	treasuryStore := treasury.New(pool, corridorStore, treasury.Config{})
	if err := treasuryStore.InitializeHDWalletSeed(ctx, mnemonic, cfg.Treasury.HDWalletSeedEncryptionKey); err != nil {
		return fmt.Errorf("adminctl: initialize hd wallet seed: %w", err)
	}

	fmt.Println("HD wallet seed initialized.")
	fmt.Printf("Mnemonic (shown once — write it down and store it offline, e.g. printed on paper in a safe):\n\n  %s\n\n", mnemonic)
	fmt.Println("This mnemonic is the only way to recover self-custodied funds if the database and its backups are ever lost. It is not stored anywhere in plaintext by this system.")
	return nil
}

func randomPassword() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("adminctl: generate password: %w", err)
	}
	return hex.EncodeToString(b), nil
}
