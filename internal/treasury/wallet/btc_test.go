package wallet

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func decodeRawBTCTxForTest(rawTxHex string) (*wire.MsgTx, error) {
	raw, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return nil, err
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return tx, nil
}

func mustTestBTCPrivKey(t *testing.T) *btcec.PrivateKey {
	t.Helper()
	seed, err := NewSeed("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	t.Cleanup(seed.Close)
	priv, err := seed.DerivePrivateKey(Bitcoin, 0)
	if err != nil {
		t.Fatalf("derive private key: %v", err)
	}
	return priv
}

func TestBTCAddress_Format(t *testing.T) {
	priv := mustTestBTCPrivKey(t)
	addr, err := btcAddress(priv)
	if err != nil {
		t.Fatalf("btc address: %v", err)
	}
	if !strings.HasPrefix(addr, "bc1q") {
		t.Fatalf("expected bech32 P2WPKH address (bc1q...), got %s", addr)
	}
}

// TestBuildAndSignBTCSweep_ScriptValid is the core correctness check for
// BTC sweep signing: build a single-input transaction spending a synthetic
// utxo and run it through txscript's actual verification engine (the same
// one a real node would use) — a genuine execution proof, not just "it
// didn't error."
func TestBuildAndSignBTCSweep_ScriptValid(t *testing.T) {
	priv := mustTestBTCPrivKey(t)
	destAddr, err := btcAddress(mustTestBTCPrivKeyAt(t, 1)) // a different address to sweep to
	if err != nil {
		t.Fatalf("dest address: %v", err)
	}

	const utxoAmount = 100_000
	const fee = 500
	utxos := []BTCUTXO{
		{TxID: strings.Repeat("11", 32), Vout: 0, AmountSats: utxoAmount},
	}

	rawTxHex, txid, err := BuildAndSignBTCSweep(priv, utxos, destAddr, fee)
	if err != nil {
		t.Fatalf("build and sign sweep: %v", err)
	}
	if txid == "" {
		t.Fatalf("expected non-empty txid")
	}

	tx, err := decodeRawBTCTxForTest(rawTxHex)
	if err != nil {
		t.Fatalf("decode raw tx: %v", err)
	}

	sourceAddr, err := btcP2WPKHAddress(priv)
	if err != nil {
		t.Fatalf("source address: %v", err)
	}
	sourceScript, err := txscript.PayToAddrScript(sourceAddr)
	if err != nil {
		t.Fatalf("source script: %v", err)
	}

	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(sourceScript, utxoAmount)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	engine, err := txscript.NewEngine(sourceScript, tx, 0, txscript.StandardVerifyFlags, nil, sigHashes, utxoAmount, prevOutFetcher)
	if err != nil {
		t.Fatalf("build script engine: %v", err)
	}
	if err := engine.Execute(); err != nil {
		t.Fatalf("script did not validate: %v", err)
	}
}

func mustTestBTCPrivKeyAt(t *testing.T, index uint32) *btcec.PrivateKey {
	t.Helper()
	seed, err := NewSeed("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	if err != nil {
		t.Fatalf("new seed: %v", err)
	}
	t.Cleanup(seed.Close)
	priv, err := seed.DerivePrivateKey(Bitcoin, index)
	if err != nil {
		t.Fatalf("derive private key: %v", err)
	}
	return priv
}
