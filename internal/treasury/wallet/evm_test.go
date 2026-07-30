package wallet

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
)

func mustTestPrivKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	seed, err := NewSeed("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	t.Cleanup(seed.Close)
	priv, err := seed.DerivePrivateKey(Ethereum, 0)
	if err != nil {
		t.Fatalf("derive private key: %v", err)
	}
	return priv
}

func TestEVMAddress_Format(t *testing.T) {
	priv := mustTestPrivKey(t)
	addr, err := evmAddress(priv)
	if err != nil {
		t.Fatalf("evm address: %v", err)
	}
	if !strings.HasPrefix(addr, "0x") || len(addr) != 42 {
		t.Fatalf("unexpected address format: %s", addr)
	}
}

func TestEVMAddress_Deterministic(t *testing.T) {
	priv := mustTestPrivKey(t)
	a1, err := evmAddress(priv)
	if err != nil {
		t.Fatalf("evm address: %v", err)
	}
	a2, err := evmAddress(priv)
	if err != nil {
		t.Fatalf("evm address: %v", err)
	}
	if a1 != a2 {
		t.Fatalf("expected deterministic address, got %s then %s", a1, a2)
	}
}

func TestEIP55Checksum_SelfConsistent(t *testing.T) {
	priv := mustTestPrivKey(t)
	addr, err := evmAddress(priv)
	if err != nil {
		t.Fatalf("evm address: %v", err)
	}
	// Re-checksumming the address's own lowercase form must reproduce it —
	// proves the checksum function is deterministic and idempotent.
	lower := strings.ToLower(addr[2:])
	lowerBytes, err := hex.DecodeString(lower)
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}
	recomputed := eip55Checksum(lowerBytes)
	if recomputed != addr {
		t.Fatalf("re-checksumming %s produced %s", addr, recomputed)
	}
}

// TestBuildAndSignEVMTx_SignatureRecoversSender is the core correctness
// check for the hand-rolled RLP encoder + EIP-1559 signing: decode the
// produced transaction back out, reconstruct the compact signature, and
// confirm it recovers a public key matching the sender's own address —
// end-to-end proof the encoding and signing agree with each other, without
// relying on an external test vector.
func TestBuildAndSignEVMTx_SignatureRecoversSender(t *testing.T) {
	priv := mustTestPrivKey(t)
	senderAddr, err := evmAddress(priv)
	if err != nil {
		t.Fatalf("evm address: %v", err)
	}

	to := [20]byte{0xaa, 0xbb, 0xcc, 0xdd}
	params := EVMTxParams{
		ChainID:              1,
		Nonce:                7,
		MaxPriorityFeePerGas: big.NewInt(1_500_000_000),
		MaxFeePerGas:         big.NewInt(30_000_000_000),
		GasLimit:             21000,
		To:                   to,
		Value:                big.NewInt(1_000_000_000_000_000),
		Data:                 nil,
	}
	rawTx, txHash, err := BuildAndSignEVMTx(priv, params)
	if err != nil {
		t.Fatalf("build and sign: %v", err)
	}
	if rawTx[0] != 0x02 {
		t.Fatalf("expected type-2 (EIP-1559) prefix 0x02, got 0x%x", rawTx[0])
	}
	if !bytes.Equal(txHash, keccak256(rawTx)) {
		t.Fatalf("returned txHash does not match keccak256(rawTx)")
	}

	decoded, err := rlpDecodeTopLevelList(rawTx[1:])
	if err != nil {
		t.Fatalf("decode signed tx: %v", err)
	}
	if len(decoded) != 12 {
		t.Fatalf("expected 12 fields in signed tx, got %d", len(decoded))
	}

	unsignedHash := keccak256(append([]byte{0x02}, encodeUnsignedEVMTx(params)...))

	yParityField, rField, sField := decoded[9], decoded[10], decoded[11]
	var yParity byte
	if len(yParityField) == 1 {
		yParity = yParityField[0]
	}
	compactSig := make([]byte, 65)
	compactSig[0] = 27 + yParity
	copy(compactSig[1+32-len(rField):33], rField)
	copy(compactSig[33+32-len(sField):65], sField)

	recoveredPub, _, err := ecdsa.RecoverCompact(compactSig, unsignedHash)
	if err != nil {
		t.Fatalf("recover public key: %v", err)
	}
	gotAddr, err := addressFromPubKey(recoveredPub)
	if err != nil {
		t.Fatalf("derive address from recovered pubkey: %v", err)
	}
	if gotAddr != senderAddr {
		t.Fatalf("recovered address %s does not match sender %s", gotAddr, senderAddr)
	}
}
