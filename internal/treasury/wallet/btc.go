package wallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// btcAddress encodes priv's public key as a BIP84 native-segwit (P2WPKH,
// bech32) address — matches v1's derivation choice.
func btcAddress(priv *btcec.PrivateKey) (string, error) {
	addr, err := btcP2WPKHAddress(priv)
	if err != nil {
		return "", err
	}
	return addr.EncodeAddress(), nil
}

func btcP2WPKHAddress(priv *btcec.PrivateKey) (*btcutil.AddressWitnessPubKeyHash, error) {
	pubKeyHash := btcutil.Hash160(priv.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	if err != nil {
		return nil, fmt.Errorf("wallet: encode btc address: %w", err)
	}
	return addr, nil
}

// BTCUTXO is one spendable output at a deposit address, as reported by a
// chain explorer (see watcher.go).
type BTCUTXO struct {
	TxID       string
	Vout       uint32
	AmountSats int64
}

// BuildAndSignBTCSweep spends every utxo (all assumed to belong to the
// single address derived from priv — a sweep only ever drains one deposit
// address at a time) to toAddress, paying feeSats out of the total and
// returning any remainder to the source address as change. Returns the raw
// signed transaction (hex) and its txid.
func BuildAndSignBTCSweep(priv *btcec.PrivateKey, utxos []BTCUTXO, toAddress string, feeSats int64) (rawTxHex string, txid string, err error) {
	if len(utxos) == 0 {
		return "", "", fmt.Errorf("wallet: no utxos to sweep")
	}

	var total int64
	for _, u := range utxos {
		total += u.AmountSats
	}
	sendSats := total - feeSats
	if sendSats <= 0 {
		return "", "", fmt.Errorf("wallet: utxo total %d insufficient to cover fee %d", total, feeSats)
	}

	sourceAddr, err := btcP2WPKHAddress(priv)
	if err != nil {
		return "", "", err
	}
	sourceScript, err := txscript.PayToAddrScript(sourceAddr)
	if err != nil {
		return "", "", fmt.Errorf("wallet: build source script: %w", err)
	}

	toAddr, err := btcutil.DecodeAddress(toAddress, &chaincfg.MainNetParams)
	if err != nil {
		return "", "", fmt.Errorf("wallet: decode destination address: %w", err)
	}
	toScript, err := txscript.PayToAddrScript(toAddr)
	if err != nil {
		return "", "", fmt.Errorf("wallet: build destination script: %w", err)
	}

	tx := wire.NewMsgTx(wire.TxVersion)
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(utxos))
	for _, u := range utxos {
		hash, err := chainhash.NewHashFromStr(u.TxID)
		if err != nil {
			return "", "", fmt.Errorf("wallet: parse utxo txid %q: %w", u.TxID, err)
		}
		outPoint := wire.NewOutPoint(hash, u.Vout)
		tx.AddTxIn(wire.NewTxIn(outPoint, nil, nil))
		prevOuts[*outPoint] = wire.NewTxOut(u.AmountSats, sourceScript)
	}

	tx.AddTxOut(wire.NewTxOut(sendSats, toScript))

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	for i, u := range utxos {
		witness, err := txscript.WitnessSignature(tx, sigHashes, i, u.AmountSats, sourceScript, txscript.SigHashAll, priv, true)
		if err != nil {
			return "", "", fmt.Errorf("wallet: sign input %d: %w", i, err)
		}
		tx.TxIn[i].Witness = witness
	}

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", "", fmt.Errorf("wallet: serialize transaction: %w", err)
	}
	return hex.EncodeToString(buf.Bytes()), tx.TxHash().String(), nil
}

// BroadcastBTCTransaction posts rawTxHex to a Blockstream-compatible
// esplora API's POST /tx endpoint, which responds with the broadcast txid
// as plain text on success.
func BroadcastBTCTransaction(ctx context.Context, client *http.Client, apiURL, rawTxHex string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/tx", bytes.NewBufferString(rawTxHex))
	if err != nil {
		return "", fmt.Errorf("wallet: build broadcast request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("wallet: broadcast btc transaction: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("wallet: read broadcast response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("wallet: broadcast rejected (status %d): %s", resp.StatusCode, body)
	}
	return string(body), nil
}
