// Package wallet is the chain-specific crypto for self-custody treasury:
// HD key derivation, address encoding, and transaction signing, isolated
// from the DB/orchestration layer in internal/treasury. Ported from v1's
// hd-wallet/watcher/sweeper (real, working code there — unlike Busha,
// which v1 never actually built).
package wallet

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/tyler-smith/go-bip39"
)

// Chain identifies one of the launch-list blockchains self-custody
// operates on. BSC reuses Ethereum's address format and derivation (both
// EVM, same secp256k1 addressing) — only the watcher's RPC/explorer
// endpoint differs between them.
type Chain string

const (
	Bitcoin  Chain = "bitcoin"
	Ethereum Chain = "ethereum"
	BSC      Chain = "bsc"
	Tron     Chain = "tron"
)

// slip44CoinType per SLIP-44 (github.com/satoshilabs/slips/blob/master/slip-0044.md).
var slip44CoinType = map[Chain]uint32{
	Bitcoin:  0,
	Ethereum: 60,
	BSC:      60,
	Tron:     195,
}

// purpose selects BIP84 (native segwit) for Bitcoin, BIP44 for the rest —
// matches v1.
var purpose = map[Chain]uint32{
	Bitcoin:  84,
	Ethereum: 44,
	BSC:      44,
	Tron:     44,
}

const (
	// depositAccount is m/purpose'/coin'/0'/0/i — reusable deposit
	// addresses, one HD index each.
	depositAccount = 0
	// gasFundingAccount is m/purpose'/coin'/1'/0/0 — a single wallet used
	// only to pre-fund gas for token sweeps. Deliberately a separate HD
	// account namespace from deposit addresses: v1 used the same account
	// namespace (just a different index) for its merchant funding
	// wallets, meaning one leaked seed compromised both purposes
	// identically via the same derivation family. This keeps that
	// separation real rather than nominal.
	gasFundingAccount = 1
)

// Seed wraps a decrypted BIP32 master seed. Only ever lives in process
// memory — Close zeroes it, mirroring v1's zero-on-destroy discipline for
// the same secret. Callers must defer Close immediately after a successful
// NewSeed.
type Seed struct {
	bytes []byte
}

// GenerateMnemonic returns a new 24-word BIP39 mnemonic (256 bits of
// entropy) — used once, by adminctl -init-wallet, to provision the seed.
func GenerateMnemonic() (string, error) {
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", fmt.Errorf("wallet: generate entropy: %w", err)
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return "", fmt.Errorf("wallet: generate mnemonic: %w", err)
	}
	return mnemonic, nil
}

// NewSeed validates mnemonic and derives the BIP32 master seed from it.
func NewSeed(mnemonic string) (*Seed, error) {
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, fmt.Errorf("wallet: invalid mnemonic")
	}
	return &Seed{bytes: bip39.NewSeed(mnemonic, "")}, nil
}

// Close zeroes the seed bytes.
func (s *Seed) Close() {
	for i := range s.bytes {
		s.bytes[i] = 0
	}
}

func (s *Seed) master() (*hdkeychain.ExtendedKey, error) {
	m, err := hdkeychain.NewMaster(s.bytes, &chaincfg.MainNetParams)
	if err != nil {
		return nil, fmt.Errorf("wallet: derive master key: %w", err)
	}
	return m, nil
}

// derivePrivateKey walks m/purpose'/coinType'/account'/0/index for chain.
func (s *Seed) derivePrivateKey(chain Chain, account, index uint32) (*btcec.PrivateKey, error) {
	p, ok := purpose[chain]
	if !ok {
		return nil, fmt.Errorf("wallet: unsupported chain %q", chain)
	}
	coinType := slip44CoinType[chain]

	key, err := s.master()
	if err != nil {
		return nil, err
	}
	for _, step := range []uint32{
		hdkeychain.HardenedKeyStart + p,
		hdkeychain.HardenedKeyStart + coinType,
		hdkeychain.HardenedKeyStart + account,
		0, // external chain (BIP44 "change" level)
		index,
	} {
		key, err = key.Derive(step)
		if err != nil {
			return nil, fmt.Errorf("wallet: derive path for %s: %w", chain, err)
		}
	}
	priv, err := key.ECPrivKey()
	if err != nil {
		return nil, fmt.Errorf("wallet: extract private key for %s: %w", chain, err)
	}
	return priv, nil
}

// DerivePrivateKey returns the deposit-address private key for
// (chain, index). Never persisted — callers use it only transiently, at
// signing time.
func (s *Seed) DerivePrivateKey(chain Chain, index uint32) (*btcec.PrivateKey, error) {
	return s.derivePrivateKey(chain, depositAccount, index)
}

// DeriveGasFundingPrivateKey returns the single gas-funding wallet's
// private key for chain (see gasFundingAccount).
func (s *Seed) DeriveGasFundingPrivateKey(chain Chain) (*btcec.PrivateKey, error) {
	return s.derivePrivateKey(chain, gasFundingAccount, 0)
}

// DeriveAddress returns the deposit address for (chain, index), formatted
// per that chain's convention (see btc.go/evm.go/tron.go).
func DeriveAddress(seed *Seed, chain Chain, index uint32) (string, error) {
	priv, err := seed.DerivePrivateKey(chain, index)
	if err != nil {
		return "", err
	}
	return addressForChain(chain, priv)
}

func addressForChain(chain Chain, priv *btcec.PrivateKey) (string, error) {
	switch chain {
	case Bitcoin:
		return btcAddress(priv)
	case Ethereum, BSC:
		return evmAddress(priv)
	case Tron:
		return tronAddress(priv)
	default:
		return "", fmt.Errorf("wallet: unsupported chain %q", chain)
	}
}
