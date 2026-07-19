package wallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"golang.org/x/crypto/sha3"
)

// erc20TransferSelector is the 4-byte selector for transfer(address,uint256)
// — keccak256("transfer(address,uint256)")[:4]. A fixed, well-known
// constant; no ABI library needed for this one method signature.
var erc20TransferSelector = [4]byte{0xa9, 0x05, 0x9c, 0xbb}

func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// evmAddress derives the EIP-55 checksummed address for priv's public key
// — identical for Ethereum and BSC (same secp256k1/Keccak256 addressing).
func evmAddress(priv *btcec.PrivateKey) (string, error) {
	return addressFromPubKey(priv.PubKey())
}

// addressFromPubKey derives the EIP-55 checksummed address for an
// already-known public key — used both by evmAddress and by tests that
// recover a public key from a signature and need to re-derive its address.
func addressFromPubKey(pub *btcec.PublicKey) (string, error) {
	serialized := pub.SerializeUncompressed() // 0x04 || X(32) || Y(32)
	hash := keccak256(serialized[1:])
	addrBytes := hash[12:] // last 20 bytes
	return eip55Checksum(addrBytes), nil
}

// eip55Checksum formats a 20-byte address per EIP-55: hex digits are
// uppercased where the corresponding nibble of keccak256(lowercase hex
// address) is >= 8.
func eip55Checksum(addr []byte) string {
	lowerHex := hex.EncodeToString(addr)
	hash := keccak256([]byte(lowerHex))

	out := make([]byte, len(lowerHex))
	for i, c := range []byte(lowerHex) {
		if c >= '0' && c <= '9' {
			out[i] = c
			continue
		}
		// nibble i of hash: high nibble for even i, low nibble for odd i.
		var nibble byte
		if i%2 == 0 {
			nibble = hash[i/2] >> 4
		} else {
			nibble = hash[i/2] & 0x0f
		}
		if nibble >= 8 {
			out[i] = c - 'a' + 'A'
		} else {
			out[i] = c
		}
	}
	return "0x" + string(out)
}

// EVMTxParams are the fields needed to build and sign one EIP-1559 (type 2)
// transaction.
type EVMTxParams struct {
	ChainID              uint64
	Nonce                uint64
	MaxPriorityFeePerGas *big.Int
	MaxFeePerGas         *big.Int
	GasLimit             uint64
	To                   [20]byte
	Value                *big.Int
	Data                 []byte
}

// encodeUnsignedEVMTx RLP-encodes the 9 fields EIP-1559 signs over:
// [chain_id, nonce, max_priority_fee_per_gas, max_fee_per_gas, gas_limit,
// destination, amount, data, access_list] — an empty access_list, since
// this system never uses one.
func encodeUnsignedEVMTx(p EVMTxParams) []byte {
	fields := [][]byte{
		rlpEncodeUint(p.ChainID),
		rlpEncodeUint(p.Nonce),
		rlpEncodeBigInt(p.MaxPriorityFeePerGas),
		rlpEncodeBigInt(p.MaxFeePerGas),
		rlpEncodeUint(p.GasLimit),
		rlpEncodeBytes(p.To[:]),
		rlpEncodeBigInt(p.Value),
		rlpEncodeBytes(p.Data),
		rlpEncodeList(), // empty access_list
	}
	return rlpEncodeList(fields...)
}

// BuildAndSignEVMTx signs an EIP-1559 transaction with priv and returns the
// raw signed transaction bytes (0x02 || rlp([...12 fields...])) and its
// transaction hash (keccak256 of those same bytes).
func BuildAndSignEVMTx(priv *btcec.PrivateKey, p EVMTxParams) (rawTx []byte, txHash []byte, err error) {
	unsigned := encodeUnsignedEVMTx(p)
	signingPayload := append([]byte{0x02}, unsigned...)
	hash := keccak256(signingPayload)

	sig := ecdsa.SignCompact(priv, hash, false)
	if len(sig) != 65 {
		return nil, nil, fmt.Errorf("wallet: unexpected signature length %d", len(sig))
	}
	yParity := uint64(sig[0]) - 27
	r := new(big.Int).SetBytes(sig[1:33])
	s := new(big.Int).SetBytes(sig[33:65])

	fields := [][]byte{
		rlpEncodeUint(p.ChainID),
		rlpEncodeUint(p.Nonce),
		rlpEncodeBigInt(p.MaxPriorityFeePerGas),
		rlpEncodeBigInt(p.MaxFeePerGas),
		rlpEncodeUint(p.GasLimit),
		rlpEncodeBytes(p.To[:]),
		rlpEncodeBigInt(p.Value),
		rlpEncodeBytes(p.Data),
		rlpEncodeList(), // empty access_list
		rlpEncodeUint(yParity),
		rlpEncodeBigInt(r),
		rlpEncodeBigInt(s),
	}
	signed := rlpEncodeList(fields...)
	rawTx = append([]byte{0x02}, signed...)
	txHash = keccak256(rawTx)
	return rawTx, txHash, nil
}

// BuildERC20TransferData ABI-encodes a transfer(address,uint256) call —
// covers both ERC20 (Ethereum) and BEP20 (BSC) USDT, which share the same
// standard transfer method signature.
func BuildERC20TransferData(to [20]byte, amount *big.Int) []byte {
	data := make([]byte, 0, 68)
	data = append(data, erc20TransferSelector[:]...)
	data = append(data, make([]byte, 12)...) // left-pad address to 32 bytes
	data = append(data, to[:]...)
	amountBytes := amount.Bytes()
	data = append(data, make([]byte, 32-len(amountBytes))...) // left-pad amount to 32 bytes
	data = append(data, amountBytes...)
	return data
}

// BroadcastEVMTransaction submits rawTx via eth_sendRawTransaction against
// an EVM JSON-RPC endpoint and returns the transaction hash the node
// assigns (should match the locally computed one).
func BroadcastEVMTransaction(ctx context.Context, client *http.Client, rpcURL string, rawTx []byte) (string, error) {
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_sendRawTransaction",
		"params":  []string{"0x" + hex.EncodeToString(rawTx)},
	})
	if err != nil {
		return "", fmt.Errorf("wallet: marshal rpc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("wallet: build rpc request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("wallet: broadcast evm transaction: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("wallet: read rpc response: %w", err)
	}

	var parsed struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("wallet: parse rpc response: %s: %w", body, err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("wallet: rpc rejected transaction: %s", parsed.Error.Message)
	}
	return parsed.Result, nil
}
