package wallet

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/fbsobreira/gotron-sdk/pkg/client"
	"github.com/fbsobreira/gotron-sdk/pkg/client/transaction"
	"github.com/fbsobreira/gotron-sdk/pkg/proto/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// tronAddress derives priv's Tron Base58Check address (0x41 version byte)
// — gotron-sdk's address package already implements this directly from a
// btcec key (same secp256k1 key type this whole package uses), so this
// wraps it rather than reimplementing Base58Check by hand.
func tronAddress(priv *btcec.PrivateKey) (string, error) {
	return address.BTCECPrivkeyToAddress(priv).String(), nil
}

// TronClient wraps a gRPC connection to a Tron full node (default
// grpc.trongrid.io:50051) for building, signing, and broadcasting
// transactions. Energy rental for TRC20 transfers (v1's approach to avoid
// burning TRX on full bandwidth/energy fees) is out of scope here — that's
// a separate paid partner integration, not pinned down yet (same reasoning
// treasury step 1 used to avoid inventing an unconfirmed partner
// integration on spec). feeLimit below is the always-working plain-fee
// fallback: pay the full fee in TRX rather than pre-acquired energy.
type TronClient struct {
	grpc *client.GrpcClient
}

// NewTronClient builds a client for grpcEndpoint (host:port, e.g.
// "grpc.trongrid.io:50051"). Call Connect before use.
func NewTronClient(grpcEndpoint string) *TronClient {
	return &TronClient{grpc: client.NewGrpcClient(grpcEndpoint)}
}

// Connect establishes the gRPC connection over TLS (TronGrid's public
// endpoints require it).
func (c *TronClient) Connect() error {
	creds := credentials.NewTLS(&tls.Config{})
	return c.grpc.Start(grpc.WithTransportCredentials(creds))
}

// SweepTRX builds, signs, and broadcasts a native TRX transfer of the
// sending address's full balance minus feeLimit (left as headroom for the
// network fee) to toAddress.
func (c *TronClient) SweepTRX(ctx context.Context, priv *btcec.PrivateKey, toAddress string, amountSun int64) (txID string, err error) {
	from, err := tronAddress(priv)
	if err != nil {
		return "", err
	}
	txExt, err := c.grpc.Transfer(from, toAddress, amountSun)
	if err != nil {
		return "", fmt.Errorf("wallet: build tron transfer: %w", err)
	}
	return c.signAndBroadcast(priv, txExt)
}

// SweepTRC20 builds, signs, and broadcasts a TRC20 token transfer (e.g.
// USDT-TRC20) of amount from the sending address to toAddress, using
// contractAddress's transfer() method. feeLimitSun bounds how much TRX the
// call may burn in fees — the plain-fee fallback described on TronClient.
func (c *TronClient) SweepTRC20(ctx context.Context, priv *btcec.PrivateKey, toAddress, contractAddress string, amount *big.Int, feeLimitSun int64) (txID string, err error) {
	from, err := tronAddress(priv)
	if err != nil {
		return "", err
	}
	txExt, err := c.grpc.TRC20Send(from, toAddress, contractAddress, amount, feeLimitSun)
	if err != nil {
		return "", fmt.Errorf("wallet: build trc20 transfer: %w", err)
	}
	return c.signAndBroadcast(priv, txExt)
}

func (c *TronClient) signAndBroadcast(priv *btcec.PrivateKey, txExt *api.TransactionExtention) (string, error) {
	if txExt.GetResult() != nil && !txExt.GetResult().GetResult() {
		return "", fmt.Errorf("wallet: tron rejected transaction build: %s", txExt.GetResult().GetMessage())
	}

	signed, err := transaction.SignTransaction(txExt.Transaction, priv)
	if err != nil {
		return "", fmt.Errorf("wallet: sign tron transaction: %w", err)
	}

	ret, err := c.grpc.Broadcast(signed)
	if err != nil {
		return "", fmt.Errorf("wallet: broadcast tron transaction: %w", err)
	}
	if !ret.GetResult() {
		return "", fmt.Errorf("wallet: tron broadcast rejected: %s", ret.GetMessage())
	}

	return hex.EncodeToString(txExt.GetTxid()), nil
}
