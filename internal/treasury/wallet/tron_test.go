package wallet

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

func mustTestTronPrivKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	seed, err := NewSeed("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	t.Cleanup(seed.Close)
	priv, err := seed.DerivePrivateKey(Tron, 0)
	if err != nil {
		t.Fatalf("derive private key: %v", err)
	}
	return priv
}

func TestTronAddress_Format(t *testing.T) {
	priv := mustTestTronPrivKey(t)
	addr, err := tronAddress(priv)
	if err != nil {
		t.Fatalf("tron address: %v", err)
	}
	// Tron's Base58Check addresses always start with "T" (0x41 version
	// byte lands there under the Bitcoin base58 alphabet).
	if !strings.HasPrefix(addr, "T") {
		t.Fatalf("expected Tron address to start with T, got %s", addr)
	}
}

func TestTronAddress_Deterministic(t *testing.T) {
	priv := mustTestTronPrivKey(t)
	a1, err := tronAddress(priv)
	if err != nil {
		t.Fatalf("tron address: %v", err)
	}
	a2, err := tronAddress(priv)
	if err != nil {
		t.Fatalf("tron address: %v", err)
	}
	if a1 != a2 {
		t.Fatalf("expected deterministic address, got %s then %s", a1, a2)
	}
}

func TestTronAddress_DiffersByIndex(t *testing.T) {
	seed, err := NewSeed("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	defer seed.Close()

	priv0, err := seed.DerivePrivateKey(Tron, 0)
	if err != nil {
		t.Fatalf("derive index 0: %v", err)
	}
	priv1, err := seed.DerivePrivateKey(Tron, 1)
	if err != nil {
		t.Fatalf("derive index 1: %v", err)
	}
	a0, _ := tronAddress(priv0)
	a1, _ := tronAddress(priv1)
	if a0 == a1 {
		t.Fatalf("expected different addresses at different derivation indices, got %s for both", a0)
	}
}
