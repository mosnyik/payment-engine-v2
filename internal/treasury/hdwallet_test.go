package treasury

import (
	"context"
	"errors"
	"testing"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

const hdWalletTestMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

var hdWalletTestEncryptionKey = []byte("01234567890123456789012345678901"[:32])

func TestInitializeAndLoadHDWalletSeed_RoundTrip(t *testing.T) {
	pool := openTestPool(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM hd_wallet_seed WHERE id = 1`)
	})

	s := New(pool, corridor.New(pool), Config{})
	ctx := context.Background()

	if err := s.InitializeHDWalletSeed(ctx, hdWalletTestMnemonic, hdWalletTestEncryptionKey); err != nil {
		t.Fatalf("initialize seed: %v", err)
	}

	loaded := New(pool, corridor.New(pool), Config{})
	if err := loaded.LoadHDWalletSeed(ctx, hdWalletTestEncryptionKey); err != nil {
		t.Fatalf("load seed: %v", err)
	}
	defer loaded.Close()

	addr1, err := wallet.DeriveAddress(loaded.seed, wallet.Ethereum, 0)
	if err != nil {
		t.Fatalf("derive address from loaded seed: %v", err)
	}

	directSeed, err := wallet.NewSeed(hdWalletTestMnemonic)
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	defer directSeed.Close()
	addr2, err := wallet.DeriveAddress(directSeed, wallet.Ethereum, 0)
	if err != nil {
		t.Fatalf("derive address from direct seed: %v", err)
	}

	if addr1 != addr2 {
		t.Fatalf("seed round-tripped through DB encryption produced a different address: %s vs %s", addr1, addr2)
	}
}

func TestInitializeHDWalletSeed_RejectsDoubleInit(t *testing.T) {
	pool := openTestPool(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM hd_wallet_seed WHERE id = 1`)
	})

	s := New(pool, corridor.New(pool), Config{})
	ctx := context.Background()

	if err := s.InitializeHDWalletSeed(ctx, hdWalletTestMnemonic, hdWalletTestEncryptionKey); err != nil {
		t.Fatalf("first initialize: %v", err)
	}
	err := s.InitializeHDWalletSeed(ctx, hdWalletTestMnemonic, hdWalletTestEncryptionKey)
	if !errors.Is(err, ErrHDWalletAlreadyInitialized) {
		t.Fatalf("expected ErrHDWalletAlreadyInitialized, got %v", err)
	}
}

func TestLoadHDWalletSeed_NotInitialized(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	err := s.LoadHDWalletSeed(context.Background(), hdWalletTestEncryptionKey)
	if !errors.Is(err, ErrHDWalletNotInitialized) {
		t.Fatalf("expected ErrHDWalletNotInitialized, got %v", err)
	}
}

func storeWithSeed(t *testing.T, s *Store) {
	t.Helper()
	seed, err := wallet.NewSeed(hdWalletTestMnemonic)
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	t.Cleanup(seed.Close)
	s.seed = seed
}

func TestAllocateOrReuseAddress_DerivesNewWhenNoneFree(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	storeWithSeed(t, s)
	ctx := context.Background()

	// Derivation only accepts real chain values, so this (like the sibling
	// test below) shares the "ethereum" hd_wallet_indices/derived_addresses
	// rows across runs against the same live database — fine here since
	// both tests only assert relative behavior (same vs. different
	// address), never a specific value.
	chain := wallet.Ethereum

	addr, err := s.allocateOrReuseAddress(ctx, chain)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if addr == "" {
		t.Fatalf("expected a non-empty address")
	}

	// No reservation was made against addr, so it's still free — a second
	// call must reuse it rather than deriving a new one.
	addr2, err := s.allocateOrReuseAddress(ctx, chain)
	if err != nil {
		t.Fatalf("allocate again: %v", err)
	}
	if addr2 != addr {
		t.Fatalf("expected reuse of the free address %s, got %s", addr, addr2)
	}
}

func TestAllocateOrReuseAddress_SkipsReservedAddress(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	storeWithSeed(t, s)
	ctx := context.Background()
	chain := wallet.Ethereum
	_, corridorID := setupCorridor(t, pool)

	addr, err := s.allocateOrReuseAddress(ctx, chain)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	// Simulate an open reservation on addr directly (below the
	// selfCustodyProvider/ReserveAddress layer, which is tested
	// separately in treasury_test.go's failover tests).
	_, err = pool.Exec(ctx,
		`INSERT INTO treasury_address_reservations
		   (corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, 'self_custody_wallet', 'self_custody', 'ETH', $2, $3, 'reserved')`,
		corridorID, string(chain), addr,
	)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}

	addr2, err := s.allocateOrReuseAddress(ctx, chain)
	if err != nil {
		t.Fatalf("allocate after reservation: %v", err)
	}
	if addr2 == addr {
		t.Fatalf("expected a new address once the first was reserved, got the same one back: %s", addr2)
	}
}

func TestSelfCustodyProvider_DisabledWithoutSeed(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	p := s.providers["self_custody_wallet"]
	if p == nil {
		t.Fatalf("expected self_custody_wallet provider to be registered")
	}
	if p.IsEnabled() {
		t.Fatalf("expected self-custody provider to be disabled before a seed is loaded")
	}
}
