package treasury

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

// usdtContractAddress is the canonical, well-known Tether USDT contract per
// chain — stable, public values (not something that changes), same
// fixed-constant treatment providers.go gives erc20TransferSelector.
var usdtContractAddress = map[wallet.Chain]string{
	wallet.Ethereum: "0xdAC17F958D2ee523a2206206994597C13D831ec7",
	wallet.BSC:      "0x55d398326f99059fF775485246999027B3197955",
	wallet.Tron:     "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
}

// ChainTransaction is one relevant transaction a chain watcher client
// reports at a deposit address.
type ChainTransaction struct {
	TxID        string
	CryptoAsset string // e.g. "BTC", "ETH", "USDT"
	Amount      decimal.Decimal
	Confirmed   bool  // the chain's own view of confirmed-vs-mempool
	BlockHeight int64 // 0 if unconfirmed; confirmations are computed by the
	// watcher orchestrator as tipHeight-BlockHeight+1, uniformly across
	// chains, rather than trusting each API's own confirmation count —
	// one policy application point instead of three.
}

// chainWatcherClient is implemented once per chain — real HTTP/gRPC
// clients below, a fake in watcher_test.go via httptest fixtures.
type chainWatcherClient interface {
	ListIncomingTransactions(ctx context.Context, address string) ([]ChainTransaction, error)
	TipHeight(ctx context.Context) (int64, error)
}

// Watcher polls currently-open self-custody reservations for deposits —
// on-demand, like v1 (only open reservations are polled, never a global
// chain scan), not a global chain scan. Ticker-driven.
type Watcher struct {
	store            *Store
	pollInterval     time.Duration
	clients          map[wallet.Chain]chainWatcherClient
	minConfirmations map[wallet.Chain]int
	// sweeper is optional — when set, a volatile-asset deposit triggers an
	// immediate sweep as soon as it's confirmed (see pollOnce). Stable
	// assets never sweep from here; they're picked up by
	// Sweeper.RunBatchSweep on its own schedule.
	sweeper *Sweeper
}

// SetSweeper wires immediate-sweep-on-confirmation for volatile assets.
// Optional — a Watcher with no Sweeper set just tracks deposit state and
// never sweeps, same as before this existed.
func (w *Watcher) SetSweeper(s *Sweeper) {
	w.sweeper = s
}

func NewWatcher(store *Store, cfg WatcherConfig, chains map[wallet.Chain]ChainConfig, httpClient *http.Client) *Watcher {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	clients := make(map[wallet.Chain]chainWatcherClient)
	minConfirmations := make(map[wallet.Chain]int)
	for chain, cc := range chains {
		if !cc.Enabled {
			continue
		}
		minConfirmations[chain] = cc.MinConfirmations
		switch chain {
		case wallet.Bitcoin:
			clients[chain] = &blockstreamClient{apiURL: cc.APIURL, client: httpClient}
		case wallet.Ethereum, wallet.BSC:
			clients[chain] = &etherscanClient{apiURL: cc.APIURL, apiKey: cc.APIKey, chain: chain, client: httpClient}
		case wallet.Tron:
			clients[chain] = &tronGridWatcherClient{apiURL: cc.APIURL, apiKey: cc.APIKey, client: httpClient}
		}
	}
	return &Watcher{
		store:            store,
		pollInterval:     cfg.PollInterval,
		clients:          clients,
		minConfirmations: minConfirmations,
	}
}

// Run polls on cfg.PollInterval until ctx is canceled. Intended to be
// started as a background goroutine from cmd/server/main.go, the same way
// rate.FetchJob.Run is.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

// pollOnce checks every currently-open watchable reservation once (both
// self-custody and tenant-provided — see listOpenWatchableReservations). A
// failure on one reservation or chain is logged and skipped, never fatal
// to the loop — matches rate's GetAllQuotes "a failing provider is
// skipped, not fatal" precedent.
func (w *Watcher) pollOnce(ctx context.Context) {
	reservations, err := w.store.listOpenWatchableReservations(ctx)
	if err != nil {
		log.Printf("treasury: watcher: list open reservations: %v", err)
		return
	}

	tipCache := make(map[wallet.Chain]int64)
	for _, r := range reservations {
		chain := wallet.Chain(r.CryptoNetwork)
		client, ok := w.clients[chain]
		if !ok {
			continue
		}

		tip, ok := tipCache[chain]
		if !ok {
			var err error
			tip, err = client.TipHeight(ctx)
			if err != nil {
				log.Printf("treasury: watcher: get tip height for %s: %v", chain, err)
				continue
			}
			tipCache[chain] = tip
		}

		txs, err := client.ListIncomingTransactions(ctx, r.Address)
		if err != nil {
			log.Printf("treasury: watcher: list transactions for %s %s: %v", chain, r.Address, err)
			continue
		}

		for _, tx := range txs {
			status := "detected"
			if tx.Confirmed {
				confirmations := tip - tx.BlockHeight + 1
				if int(confirmations) >= w.minConfirmations[chain] {
					status = "confirmed"
				}
			}
			payload, _ := json.Marshal(tx)
			if err := w.store.recordDepositTransition(ctx, r.ID, status, tx.Amount, tx.TxID, payload); err != nil {
				log.Printf("treasury: watcher: record deposit %s: %v", tx.TxID, err)
				continue
			}

			switch r.CustodyType {
			case CustodyTypeSelf:
				// Sweeping calls wallet.DerivePrivateKey — only ever safe
				// for a self-custody reservation, never tenant-provided
				// (we hold no key for those). This check is deliberate,
				// not incidental: once tenant-provided rows started
				// flowing through this same loop, an unconditional sweep
				// trigger would have tried to derive a key for an address
				// this package never derived.
				if status == "confirmed" && w.sweeper != nil && !isStableAsset(tx.CryptoAsset) {
					if err := w.sweeper.ExecuteSweep(ctx, r.ID); err != nil {
						log.Printf("treasury: watcher: immediate sweep for reservation %s: %v", r.ID, err)
					}
				}
			case CustodyTypeTenantProvided:
				// No sweep, ever — monitoring + webhook confirmation only
				// (the platform holds no key for a tenant-supplied
				// wallet). Notify on both a fresh detection and a
				// confirmation.
				if w.store.tenantWebhooks != nil {
					if err := w.store.notifyTenant(ctx, r.TenantID, "deposit."+status, r, tx); err != nil {
						log.Printf("treasury: watcher: notify tenant for reservation %s: %v", r.ID, err)
					}
				}
			}
		}
	}
}

// listOpenWatchableReservations returns every reservation the watcher
// should poll on-chain: self-custody (we derived the address, we sweep
// it) and tenant-provided (we monitor it, we never sweep it — see
// pollOnce's custody-type switch). Busha (partner_custodied) is excluded
// — it's webhook-driven from Busha's side, never polled by us.
func (s *Store) listOpenWatchableReservations(ctx context.Context) ([]AddressReservation, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, tenant_id, corridor_id, provider_name, custody_type, crypto_asset, crypto_network,
		        address, address_tag, provider_reference, status, reserved_at, released_at
		 FROM treasury_address_reservations
		 WHERE status = 'reserved' AND custody_type IN ('self_custody', 'tenant_provided')`,
	)
	if err != nil {
		return nil, fmt.Errorf("treasury: list open watchable reservations: %w", err)
	}
	defer rows.Close()

	var out []AddressReservation
	for rows.Next() {
		var r AddressReservation
		var custodyType string
		if err := rows.Scan(&r.ID, &r.TenantID, &r.CorridorID, &r.ProviderName, &custodyType, &r.CryptoAsset, &r.CryptoNetwork,
			&r.Address, &r.AddressTag, &r.ProviderReference, &r.Status, &r.ReservedAt, &r.ReleasedAt); err != nil {
			return nil, fmt.Errorf("treasury: scan reservation: %w", err)
		}
		r.CustodyType = CustodyType(custodyType)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("treasury: list open watchable reservations: %w", err)
	}
	return out, nil
}

// --- Bitcoin: Blockstream / any Esplora-compatible REST API ---

type blockstreamClient struct {
	apiURL string
	client *http.Client
}

type blockstreamTx struct {
	TxID   string `json:"txid"`
	Status struct {
		Confirmed   bool  `json:"confirmed"`
		BlockHeight int64 `json:"block_height"`
	} `json:"status"`
	Vout []struct {
		ScriptPubKeyAddress string `json:"scriptpubkey_address"`
		Value               int64  `json:"value"` // satoshis
	} `json:"vout"`
}

func (c *blockstreamClient) ListIncomingTransactions(ctx context.Context, address string) ([]ChainTransaction, error) {
	body, err := c.get(ctx, "/address/"+address+"/txs")
	if err != nil {
		return nil, err
	}
	var txs []blockstreamTx
	if err := json.Unmarshal(body, &txs); err != nil {
		return nil, fmt.Errorf("treasury: parse blockstream response: %w", err)
	}

	var out []ChainTransaction
	for _, tx := range txs {
		var total int64
		for _, v := range tx.Vout {
			if v.ScriptPubKeyAddress == address {
				total += v.Value
			}
		}
		if total == 0 {
			continue // no output to this address — not a deposit to it
		}
		out = append(out, ChainTransaction{
			TxID:        tx.TxID,
			CryptoAsset: "BTC",
			Amount:      decimal.New(total, -8),
			Confirmed:   tx.Status.Confirmed,
			BlockHeight: tx.Status.BlockHeight,
		})
	}
	return out, nil
}

type blockstreamUTXO struct {
	TxID   string `json:"txid"`
	Vout   uint32 `json:"vout"`
	Value  int64  `json:"value"`
	Status struct {
		Confirmed bool `json:"confirmed"`
	} `json:"status"`
}

// ListUTXOs returns address's spendable (confirmed) outputs, for sweep.go
// to build a sweep transaction from.
func (c *blockstreamClient) ListUTXOs(ctx context.Context, address string) ([]wallet.BTCUTXO, error) {
	body, err := c.get(ctx, "/address/"+address+"/utxo")
	if err != nil {
		return nil, err
	}
	var utxos []blockstreamUTXO
	if err := json.Unmarshal(body, &utxos); err != nil {
		return nil, fmt.Errorf("treasury: parse blockstream utxo response: %w", err)
	}
	var out []wallet.BTCUTXO
	for _, u := range utxos {
		if !u.Status.Confirmed {
			continue
		}
		out = append(out, wallet.BTCUTXO{TxID: u.TxID, Vout: u.Vout, AmountSats: u.Value})
	}
	return out, nil
}

func (c *blockstreamClient) TipHeight(ctx context.Context) (int64, error) {
	body, err := c.get(ctx, "/blocks/tip/height")
	if err != nil {
		return 0, err
	}
	height, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("treasury: parse blockstream tip height %q: %w", body, err)
	}
	return height, nil
}

func (c *blockstreamClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("treasury: call blockstream %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("treasury: read blockstream response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("treasury: blockstream %s returned status %d: %s", path, resp.StatusCode, body)
	}
	return body, nil
}

// --- Ethereum/BSC: Etherscan V2 unified API (chainid selects the chain) ---

var etherscanChainID = map[wallet.Chain]int{
	wallet.Ethereum: 1,
	wallet.BSC:      56,
}

type etherscanClient struct {
	apiURL string // defaults to https://api.etherscan.io/v2/api if empty
	apiKey string
	chain  wallet.Chain
	client *http.Client
}

type etherscanTxListResponse struct {
	Status string              `json:"status"`
	Result []etherscanNativeTx `json:"result"`
}

type etherscanNativeTx struct {
	Hash        string `json:"hash"`
	To          string `json:"to"`
	Value       string `json:"value"` // wei, decimal string
	BlockNumber string `json:"blockNumber"`
	IsError     string `json:"isError"`
}

type etherscanTokenTxResponse struct {
	Status string             `json:"status"`
	Result []etherscanTokenTx `json:"result"`
}

type etherscanTokenTx struct {
	Hash            string `json:"hash"`
	To              string `json:"to"`
	ContractAddress string `json:"contractAddress"`
	Value           string `json:"value"` // raw token units, decimal string
	TokenSymbol     string `json:"tokenSymbol"`
	TokenDecimal    string `json:"tokenDecimal"`
	BlockNumber     string `json:"blockNumber"`
}

func (c *etherscanClient) baseURL() string {
	if c.apiURL != "" {
		return c.apiURL
	}
	return "https://api.etherscan.io/v2/api"
}

func (c *etherscanClient) ListIncomingTransactions(ctx context.Context, address string) ([]ChainTransaction, error) {
	var out []ChainTransaction

	nativeSymbol := "ETH"
	if c.chain == wallet.BSC {
		nativeSymbol = "BNB"
	}

	nativeURL := fmt.Sprintf("%s?chainid=%d&module=account&action=txlist&address=%s&sort=desc&apikey=%s",
		c.baseURL(), etherscanChainID[c.chain], address, c.apiKey)
	var nativeResp etherscanTxListResponse
	if err := c.getJSON(ctx, nativeURL, &nativeResp); err != nil {
		return nil, err
	}
	for _, tx := range nativeResp.Result {
		if !strings.EqualFold(tx.To, address) || tx.IsError != "0" {
			continue
		}
		amount, blockHeight, err := parseWeiAndBlock(tx.Value, tx.BlockNumber, 18)
		if err != nil {
			return nil, err
		}
		out = append(out, ChainTransaction{TxID: tx.Hash, CryptoAsset: nativeSymbol, Amount: amount, Confirmed: true, BlockHeight: blockHeight})
	}

	contract, ok := usdtContractAddress[c.chain]
	if ok {
		tokenURL := fmt.Sprintf("%s?chainid=%d&module=account&action=tokentx&address=%s&contractaddress=%s&sort=desc&apikey=%s",
			c.baseURL(), etherscanChainID[c.chain], address, contract, c.apiKey)
		var tokenResp etherscanTokenTxResponse
		if err := c.getJSON(ctx, tokenURL, &tokenResp); err != nil {
			return nil, err
		}
		for _, tx := range tokenResp.Result {
			if !strings.EqualFold(tx.To, address) {
				continue
			}
			decimals, err := strconv.Atoi(tx.TokenDecimal)
			if err != nil {
				return nil, fmt.Errorf("treasury: parse token decimals %q: %w", tx.TokenDecimal, err)
			}
			amount, blockHeight, err := parseWeiAndBlock(tx.Value, tx.BlockNumber, int32(decimals))
			if err != nil {
				return nil, err
			}
			out = append(out, ChainTransaction{TxID: tx.Hash, CryptoAsset: tx.TokenSymbol, Amount: amount, Confirmed: true, BlockHeight: blockHeight})
		}
	}

	// Native transactions are only ever reported once confirmed by
	// Etherscan's indexer in practice for this system's polling cadence;
	// Confirmed is set true above since txlist/tokentx don't return
	// pending transactions the way a mempool feed would.
	return out, nil
}

func parseWeiAndBlock(valueStr, blockNumberStr string, decimals int32) (decimal.Decimal, int64, error) {
	raw, ok := new(big.Int).SetString(valueStr, 10)
	if !ok {
		return decimal.Decimal{}, 0, fmt.Errorf("treasury: parse value %q", valueStr)
	}
	amount := decimal.NewFromBigInt(raw, -decimals)
	blockHeight, err := strconv.ParseInt(blockNumberStr, 10, 64)
	if err != nil {
		return decimal.Decimal{}, 0, fmt.Errorf("treasury: parse block number %q: %w", blockNumberStr, err)
	}
	return amount, blockHeight, nil
}

func (c *etherscanClient) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("treasury: call etherscan: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("treasury: read etherscan response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("treasury: etherscan returned status %d: %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("treasury: parse etherscan response: %s: %w", body, err)
	}
	return nil
}

// NonceAt returns address's next transaction count (nonce), for sweep.go
// to build an EVM sweep transaction.
func (c *etherscanClient) NonceAt(ctx context.Context, address string) (uint64, error) {
	url := fmt.Sprintf("%s?chainid=%d&module=proxy&action=eth_getTransactionCount&address=%s&tag=latest&apikey=%s",
		c.baseURL(), etherscanChainID[c.chain], address, c.apiKey)
	var resp struct {
		Result string `json:"result"`
	}
	if err := c.getJSON(ctx, url, &resp); err != nil {
		return 0, err
	}
	nonce, err := strconv.ParseUint(strings.TrimPrefix(resp.Result, "0x"), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("treasury: parse eth_getTransactionCount %q: %w", resp.Result, err)
	}
	return nonce, nil
}

// SuggestedGasPrice returns the network's current gas price (wei), used as
// a simple (non-EIP-1559-priority-fee-aware) basis for both
// maxFeePerGas/maxPriorityFeePerGas in sweep.go — adequate for this
// system's sweep transactions, which aren't latency-sensitive the way a
// user-facing transaction might be.
func (c *etherscanClient) SuggestedGasPrice(ctx context.Context) (decimal.Decimal, error) {
	url := fmt.Sprintf("%s?chainid=%d&module=proxy&action=eth_gasPrice&apikey=%s", c.baseURL(), etherscanChainID[c.chain], c.apiKey)
	var resp struct {
		Result string `json:"result"`
	}
	if err := c.getJSON(ctx, url, &resp); err != nil {
		return decimal.Decimal{}, err
	}
	wei, ok := new(big.Int).SetString(strings.TrimPrefix(resp.Result, "0x"), 16)
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("treasury: parse eth_gasPrice %q", resp.Result)
	}
	return decimal.NewFromBigInt(wei, 0), nil
}

// NativeBalance returns address's native asset balance (ETH/BNB), for
// sweep.go's gas pre-funding check before a token sweep.
func (c *etherscanClient) NativeBalance(ctx context.Context, address string) (decimal.Decimal, error) {
	url := fmt.Sprintf("%s?chainid=%d&module=account&action=balance&address=%s&tag=latest&apikey=%s",
		c.baseURL(), etherscanChainID[c.chain], address, c.apiKey)
	var resp struct {
		Result string `json:"result"` // decimal string, wei
	}
	if err := c.getJSON(ctx, url, &resp); err != nil {
		return decimal.Decimal{}, err
	}
	wei, ok := new(big.Int).SetString(resp.Result, 10)
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("treasury: parse native balance %q", resp.Result)
	}
	return decimal.NewFromBigInt(wei, -18), nil
}

func (c *etherscanClient) TipHeight(ctx context.Context) (int64, error) {
	url := fmt.Sprintf("%s?chainid=%d&module=proxy&action=eth_blockNumber&apikey=%s", c.baseURL(), etherscanChainID[c.chain], c.apiKey)
	var resp struct {
		Result string `json:"result"`
	}
	if err := c.getJSON(ctx, url, &resp); err != nil {
		return 0, err
	}
	hexStr := strings.TrimPrefix(resp.Result, "0x")
	height, err := strconv.ParseInt(hexStr, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("treasury: parse eth_blockNumber %q: %w", resp.Result, err)
	}
	return height, nil
}

// --- Tron: TronGrid REST API (separate from wallet.TronClient's gRPC
// connection, which is used only for signing/broadcasting sweeps) ---

type tronGridWatcherClient struct {
	apiURL string // defaults to https://api.trongrid.io if empty
	apiKey string
	client *http.Client
}

func (c *tronGridWatcherClient) baseURL() string {
	if c.apiURL != "" {
		return c.apiURL
	}
	return "https://api.trongrid.io"
}

type tronGridTRC20Response struct {
	Data []struct {
		TransactionID string `json:"transaction_id"`
		To            string `json:"to"`
		Value         string `json:"value"` // raw token units, decimal string
		TokenInfo     struct {
			Symbol   string `json:"symbol"`
			Decimals int    `json:"decimals"`
		} `json:"token_info"`
		BlockTimestamp int64 `json:"block_timestamp"`
	} `json:"data"`
}

// ListIncomingTransactions covers USDT-TRC20 transfers, whose response
// shape was confirmed against TronGrid's live public API. Native TRX
// transfer parsing (raw_data.contract[].type == "TransferContract") is not
// implemented here yet — TronGrid's native /transactions endpoint requires
// decoding protobuf-adjacent hex fields this package didn't need to solve
// for USDT-TRC20 (the launch corridor's Tron asset), and doing it
// correctly deserves its own dedicated pass rather than a guessed shape in
// this one. TODO: add native TRX deposit detection.
func (c *tronGridWatcherClient) ListIncomingTransactions(ctx context.Context, address string) ([]ChainTransaction, error) {
	contract := usdtContractAddress[wallet.Tron]
	url := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?contract_address=%s&limit=200", c.baseURL(), address, contract)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("treasury: call trongrid: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("treasury: read trongrid response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("treasury: trongrid returned status %d: %s", resp.StatusCode, body)
	}

	var parsed tronGridTRC20Response
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("treasury: parse trongrid response: %s: %w", body, err)
	}

	var out []ChainTransaction
	for _, tx := range parsed.Data {
		if tx.To != address {
			continue
		}
		raw, ok := new(big.Int).SetString(tx.Value, 10)
		if !ok {
			return nil, fmt.Errorf("treasury: parse trc20 value %q", tx.Value)
		}
		amount := decimal.NewFromBigInt(raw, -int32(tx.TokenInfo.Decimals))
		out = append(out, ChainTransaction{
			TxID:        tx.TransactionID,
			CryptoAsset: tx.TokenInfo.Symbol,
			Amount:      amount,
			Confirmed:   true, // TronGrid's confirmed-transactions index only lists finalized txs
			BlockHeight: 0,    // not returned by this endpoint; Tron's ~3s block time and this system's min-confirmation policy mean this is a known gap — TODO alongside native TRX support
		})
	}
	return out, nil
}

// NativeBalance returns address's TRX balance, for sweep.go's gas
// pre-funding check before a TRC20 sweep.
func (c *tronGridWatcherClient) NativeBalance(ctx context.Context, address string) (decimal.Decimal, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/v1/accounts/"+address, nil)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if c.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("treasury: call trongrid account: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return decimal.Decimal{}, err
	}
	var parsed struct {
		Data []struct {
			Balance int64 `json:"balance"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return decimal.Decimal{}, fmt.Errorf("treasury: parse trongrid account response: %s: %w", body, err)
	}
	if len(parsed.Data) == 0 {
		return decimal.Zero, nil // account never seen on-chain yet — zero balance
	}
	return decimal.New(parsed.Data[0].Balance, -6), nil
}

func (c *tronGridWatcherClient) TipHeight(ctx context.Context) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/wallet/getnowblock", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("treasury: call trongrid getnowblock: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var parsed struct {
		BlockHeader struct {
			RawData struct {
				Number int64 `json:"number"`
			} `json:"raw_data"`
		} `json:"block_header"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("treasury: parse trongrid getnowblock response: %s: %w", body, err)
	}
	return parsed.BlockHeader.RawData.Number, nil
}
