package treasury

import (
	"context"
	"errors"
	"testing"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

func TestRegisterTenantCustomWallet_RejectsMalformedAddress(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	tenantID := createTestTenant(t, pool)
	ctx := context.Background()

	cases := []struct {
		chain   wallet.Chain
		address string
	}{
		{wallet.Bitcoin, "not-a-bitcoin-address"},
		{wallet.Ethereum, "0xnothex"},
		{wallet.Ethereum, "0x1234"}, // too short
		{wallet.Tron, "not-a-tron-address"},
	}
	for _, tc := range cases {
		err := s.RegisterTenantCustomWallet(ctx, tenantID, tc.chain, tc.address, "")
		if err == nil {
			t.Fatalf("expected %s address %q to be rejected", tc.chain, tc.address)
		}
	}
}

func TestRegisterTenantCustomWallet_AcceptsValidAddressAndRoundTrips(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	tenantID := createTestTenant(t, pool)
	ctx := context.Background()

	const validEthAddr = "0x67c6b441b309ff5716f1929d94d0da507b16eab8" // all-lowercase — no checksum required
	if err := s.RegisterTenantCustomWallet(ctx, tenantID, wallet.Ethereum, validEthAddr, "memo-1"); err != nil {
		t.Fatalf("register: %v", err)
	}

	addr, tag, err := s.getTenantCustomWallet(ctx, tenantID, wallet.Ethereum)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if addr != validEthAddr || tag != "memo-1" {
		t.Fatalf("expected %s/memo-1, got %s/%s", validEthAddr, addr, tag)
	}

	// Re-registering (e.g. tenant changes their wallet) must update in
	// place, not error or create a second row.
	const secondAddr = "0x1111111111111111111111111111111111111111"
	if err := s.RegisterTenantCustomWallet(ctx, tenantID, wallet.Ethereum, secondAddr, ""); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	addr2, _, err := s.getTenantCustomWallet(ctx, tenantID, wallet.Ethereum)
	if err != nil {
		t.Fatalf("get after re-register: %v", err)
	}
	if addr2 != secondAddr {
		t.Fatalf("expected updated address %s, got %s", secondAddr, addr2)
	}
}

func TestGetTenantCustomWallet_NotRegistered(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	tenantID := createTestTenant(t, pool)

	_, _, err := s.getTenantCustomWallet(context.Background(), tenantID, wallet.Tron)
	if !errors.Is(err, ErrTenantWalletNotRegistered) {
		t.Fatalf("expected ErrTenantWalletNotRegistered, got %v", err)
	}
}

func TestTenantProvidedWalletProvider_ReserveAddress(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	tenantID := createTestTenant(t, pool)
	ctx := context.Background()

	const validTronAddr = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	if err := s.RegisterTenantCustomWallet(ctx, tenantID, wallet.Tron, validTronAddr, ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	p := s.providers["tenant_provided_wallet"]
	if p == nil {
		t.Fatalf("expected tenant_provided_wallet provider to be registered")
	}
	if !p.IsEnabled() {
		t.Fatalf("expected tenant-provided-wallet provider to always be enabled")
	}
	if p.CustodyType() != CustodyTypeTenantProvided {
		t.Fatalf("expected CustodyTypeTenantProvided, got %s", p.CustodyType())
	}

	got, err := p.ReserveAddress(ctx, tenantID, "TRX", "tron")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	if got.Address != validTronAddr {
		t.Fatalf("expected %s, got %s", validTronAddr, got.Address)
	}

	otherTenant := createTestTenant(t, pool)
	_, err = p.ReserveAddress(ctx, otherTenant, "TRX", "tron")
	if !errors.Is(err, ErrTenantWalletNotRegistered) {
		t.Fatalf("expected ErrTenantWalletNotRegistered for a tenant with no registered wallet, got %v", err)
	}
}
