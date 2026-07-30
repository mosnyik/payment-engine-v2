package wallet

import (
	"bytes"
	"testing"

	"github.com/tyler-smith/go-bip39"
)

const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

func TestGenerateMnemonic_Valid(t *testing.T) {
	m, err := GenerateMnemonic()
	if err != nil {
		t.Fatalf("generate mnemonic: %v", err)
	}
	if !bip39.IsMnemonicValid(m) {
		t.Fatalf("generated mnemonic failed validation: %s", m)
	}
}

func TestGenerateMnemonic_Unique(t *testing.T) {
	m1, err := GenerateMnemonic()
	if err != nil {
		t.Fatalf("generate mnemonic: %v", err)
	}
	m2, err := GenerateMnemonic()
	if err != nil {
		t.Fatalf("generate mnemonic: %v", err)
	}
	if m1 == m2 {
		t.Fatalf("expected two independently generated mnemonics to differ")
	}
}

func TestNewSeed_RejectsInvalidMnemonic(t *testing.T) {
	_, err := NewSeed("not a valid mnemonic at all")
	if err == nil {
		t.Fatalf("expected error for invalid mnemonic")
	}
}

func TestNewSeed_Deterministic(t *testing.T) {
	s1, err := NewSeed(testMnemonic)
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	defer s1.Close()
	s2, err := NewSeed(testMnemonic)
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	defer s2.Close()

	addr1, err := DeriveAddress(s1, Ethereum, 0)
	if err != nil {
		t.Fatalf("derive address: %v", err)
	}
	addr2, err := DeriveAddress(s2, Ethereum, 0)
	if err != nil {
		t.Fatalf("derive address: %v", err)
	}
	if addr1 != addr2 {
		t.Fatalf("same mnemonic produced different addresses: %s vs %s", addr1, addr2)
	}
}

func TestSeed_Close_Zeroes(t *testing.T) {
	s, err := NewSeed(testMnemonic)
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	allZero := true
	for _, b := range s.bytes {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatalf("seed bytes were already zero before Close")
	}
	s.Close()
	for i, b := range s.bytes {
		if b != 0 {
			t.Fatalf("seed byte %d not zeroed after Close: %x", i, b)
		}
	}
}

func TestDeriveAddress_DiffersAcrossChains(t *testing.T) {
	seed, err := NewSeed(testMnemonic)
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	defer seed.Close()

	// BSC is deliberately excluded here — it shares Ethereum's derivation
	// and address format by design (see TestDeriveAddress_EthereumAndBSCShareFormat).
	seen := map[string]Chain{}
	for _, chain := range []Chain{Bitcoin, Ethereum, Tron} {
		addr, err := DeriveAddress(seed, chain, 0)
		if err != nil {
			t.Fatalf("derive address for %s: %v", chain, err)
		}
		if addr == "" {
			t.Fatalf("empty address for %s", chain)
		}
		if prev, ok := seen[addr]; ok {
			t.Fatalf("chain %s produced the same address as %s: %s", chain, prev, addr)
		}
		seen[addr] = chain
	}
}

// Ethereum and BSC deliberately share derivation and address format (both
// EVM) — confirm that's actually true, not an accident.
func TestDeriveAddress_EthereumAndBSCShareFormat(t *testing.T) {
	seed, err := NewSeed(testMnemonic)
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	defer seed.Close()

	ethAddr, err := DeriveAddress(seed, Ethereum, 3)
	if err != nil {
		t.Fatalf("derive ethereum address: %v", err)
	}
	bscAddr, err := DeriveAddress(seed, BSC, 3)
	if err != nil {
		t.Fatalf("derive bsc address: %v", err)
	}
	if ethAddr != bscAddr {
		t.Fatalf("expected ethereum and bsc to share the same address at the same index, got %s vs %s", ethAddr, bscAddr)
	}
}

func TestDerivePrivateKey_DiffersByIndex(t *testing.T) {
	seed, err := NewSeed(testMnemonic)
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	defer seed.Close()

	k0, err := seed.DerivePrivateKey(Bitcoin, 0)
	if err != nil {
		t.Fatalf("derive index 0: %v", err)
	}
	k1, err := seed.DerivePrivateKey(Bitcoin, 1)
	if err != nil {
		t.Fatalf("derive index 1: %v", err)
	}
	if bytes.Equal(k0.Serialize(), k1.Serialize()) {
		t.Fatalf("expected different keys at different indices")
	}
}

func TestDeriveGasFundingPrivateKey_DiffersFromDepositAccount(t *testing.T) {
	seed, err := NewSeed(testMnemonic)
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	defer seed.Close()

	deposit, err := seed.DerivePrivateKey(Ethereum, 0)
	if err != nil {
		t.Fatalf("derive deposit key: %v", err)
	}
	gasFunding, err := seed.DeriveGasFundingPrivateKey(Ethereum)
	if err != nil {
		t.Fatalf("derive gas funding key: %v", err)
	}
	if bytes.Equal(deposit.Serialize(), gasFunding.Serialize()) {
		t.Fatalf("gas-funding wallet must use a separate HD account from deposit addresses")
	}
}
