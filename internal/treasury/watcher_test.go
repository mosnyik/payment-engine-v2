package treasury

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

// blockstreamFixture is a trimmed, verbatim response captured from the
// live Blockstream public API (blockstream.info/api/address/.../txs) for
// address bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4 — not hand-constructed.
const blockstreamFixture = `[
  {
    "txid": "c3afd5b4a8b9a866680613f2fb0798f211326395804ac69b5b7c40b3e563dea2",
    "vout": [
      {
        "scriptpubkey_address": "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
        "value": 294
      }
    ],
    "status": {
      "confirmed": true,
      "block_height": 957326,
      "block_hash": "00000000000000000001219f91f390cf6fc1bc962ca73edba5a96cebcc30bc91",
      "block_time": 1783614584
    }
  },
  {
    "txid": "unrelated0000000000000000000000000000000000000000000000000000",
    "vout": [
      {
        "scriptpubkey_address": "bc1qsomeoneelse00000000000000000000000000",
        "value": 500000
      }
    ],
    "status": { "confirmed": false, "block_height": 0 }
  }
]`

func TestBlockstreamClient_ListIncomingTransactions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(blockstreamFixture))
	}))
	defer server.Close()

	c := &blockstreamClient{apiURL: server.URL, client: server.Client()}
	txs, err := c.ListIncomingTransactions(context.Background(), "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4")
	if err != nil {
		t.Fatalf("list transactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("expected 1 matching transaction (the other has no output to our address), got %d", len(txs))
	}
	tx := txs[0]
	if tx.TxID != "c3afd5b4a8b9a866680613f2fb0798f211326395804ac69b5b7c40b3e563dea2" {
		t.Fatalf("unexpected txid: %s", tx.TxID)
	}
	if !tx.Amount.Equal(decimal.New(294, -8)) {
		t.Fatalf("expected amount 294 satoshis (2.94e-6 BTC), got %s", tx.Amount)
	}
	if !tx.Confirmed || tx.BlockHeight != 957326 {
		t.Fatalf("expected confirmed at height 957326, got confirmed=%v height=%d", tx.Confirmed, tx.BlockHeight)
	}
}

func TestBlockstreamClient_TipHeight(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("959017"))
	}))
	defer server.Close()

	c := &blockstreamClient{apiURL: server.URL, client: server.Client()}
	height, err := c.TipHeight(context.Background())
	if err != nil {
		t.Fatalf("tip height: %v", err)
	}
	if height != 959017 {
		t.Fatalf("expected 959017, got %d", height)
	}
}

func TestEtherscanClient_ListIncomingTransactions(t *testing.T) {
	const address = "0x67c6B441b309ff5716F1929D94D0Da507B16eaB8"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("action") {
		case "txlist":
			w.Write([]byte(`{"status":"1","result":[
				{"hash":"0xnative1","to":"` + address + `","value":"1000000000000000000","blockNumber":"18000000","isError":"0"},
				{"hash":"0xerrored","to":"` + address + `","value":"5000000000000000000","blockNumber":"18000001","isError":"1"}
			]}`))
		case "tokentx":
			w.Write([]byte(`{"status":"1","result":[
				{"hash":"0xtoken1","to":"` + address + `","contractAddress":"0xdac17f958d2ee523a2206206994597c13d831ec7","value":"250000000","tokenSymbol":"USDT","tokenDecimal":"6","blockNumber":"18000010"}
			]}`))
		}
	}))
	defer server.Close()

	c := &etherscanClient{apiURL: server.URL, apiKey: "test", chain: wallet.Ethereum, client: server.Client()}
	txs, err := c.ListIncomingTransactions(context.Background(), address)
	if err != nil {
		t.Fatalf("list transactions: %v", err)
	}
	if len(txs) != 2 {
		t.Fatalf("expected 2 transactions (native + token; errored native tx excluded), got %d", len(txs))
	}

	var native, token *ChainTransaction
	for i := range txs {
		switch txs[i].CryptoAsset {
		case "ETH":
			native = &txs[i]
		case "USDT":
			token = &txs[i]
		}
	}
	if native == nil {
		t.Fatalf("expected a native ETH transaction")
	}
	if !native.Amount.Equal(decimal.New(1, 0)) {
		t.Fatalf("expected native amount 1 ETH, got %s", native.Amount)
	}
	if token == nil {
		t.Fatalf("expected a USDT token transaction")
	}
	if !token.Amount.Equal(decimal.New(250, 0)) {
		t.Fatalf("expected token amount 250 USDT (250000000 / 1e6), got %s", token.Amount)
	}
}

func TestEtherscanClient_TipHeight(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x112a880"}`))
	}))
	defer server.Close()

	c := &etherscanClient{apiURL: server.URL, chain: wallet.Ethereum, client: server.Client()}
	height, err := c.TipHeight(context.Background())
	if err != nil {
		t.Fatalf("tip height: %v", err)
	}
	if height != 0x112a880 {
		t.Fatalf("expected %d, got %d", int64(0x112a880), height)
	}
}

// tronGridTRC20Fixture is a verbatim response captured from the live
// TronGrid public API (api.trongrid.io/v1/accounts/.../transactions/trc20)
// for the USDT-TRC20 contract address itself.
const tronGridTRC20Fixture = `{"data":[{"transaction_id":"77ff0c0819cc39b3d24c98a3e94c1789f9e4376509aaadc82894896474c0e6b2","token_info":{"symbol":"USDT","address":"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t","decimals":6,"name":"Tether USD"},"block_timestamp":1784641290000,"from":"TNtbiqvanUnm4YHK9SYzRGR8ktiHRduDbM","to":"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t","type":"Transfer","value":"195170000"}],"success":true}`

func TestTronGridWatcherClient_ListIncomingTransactions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tronGridTRC20Fixture))
	}))
	defer server.Close()

	c := &tronGridWatcherClient{apiURL: server.URL, client: server.Client()}
	txs, err := c.ListIncomingTransactions(context.Background(), "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	if err != nil {
		t.Fatalf("list transactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("expected 1 transaction, got %d", len(txs))
	}
	tx := txs[0]
	if tx.CryptoAsset != "USDT" {
		t.Fatalf("expected USDT, got %s", tx.CryptoAsset)
	}
	if !tx.Amount.Equal(decimal.New(195170000, -6)) {
		t.Fatalf("expected amount 195.17 USDT, got %s", tx.Amount)
	}
	if !tx.Confirmed {
		t.Fatalf("expected confirmed=true")
	}
}

const tronGridNowBlockFixture = `{"blockID":"00000000050bc53ee88cdc15641d55a96e6617e773b9d3d6d32d423d859cefa4","block_header":{"raw_data":{"number":84657470}}}`

func TestTronGridWatcherClient_TipHeight(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(tronGridNowBlockFixture))
	}))
	defer server.Close()

	c := &tronGridWatcherClient{apiURL: server.URL, client: server.Client()}
	height, err := c.TipHeight(context.Background())
	if err != nil {
		t.Fatalf("tip height: %v", err)
	}
	if height != 84657470 {
		t.Fatalf("expected 84657470, got %d", height)
	}
}

// fakeChainWatcherClient lets pollOnce's orchestration be tested without
// any network dependency.
type fakeChainWatcherClient struct {
	txs []ChainTransaction
	tip int64
	err error
}

func (f *fakeChainWatcherClient) ListIncomingTransactions(ctx context.Context, address string) ([]ChainTransaction, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.txs, nil
}

func (f *fakeChainWatcherClient) TipHeight(ctx context.Context) (int64, error) {
	return f.tip, nil
}

func TestWatcher_PollOnce_ConfirmsWhenThresholdMet(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	storeWithSeed(t, s)
	ctx := context.Background()

	_, corridorID := setupCorridor(t, pool)
	addr, err := s.allocateOrReuseAddress(ctx, wallet.Ethereum)
	if err != nil {
		t.Fatalf("allocate address: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO treasury_address_reservations
		   (corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, 'self_custody_wallet', 'self_custody', 'ETH', 'ethereum', $2, 'reserved')`,
		corridorID, addr,
	)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}

	w := &Watcher{
		store:        s,
		pollInterval: time.Second,
		clients: map[wallet.Chain]chainWatcherClient{
			wallet.Ethereum: &fakeChainWatcherClient{
				tip: 1000,
				txs: []ChainTransaction{
					{TxID: "0xdeposit1", CryptoAsset: "ETH", Amount: decimal.New(2, 0), Confirmed: true, BlockHeight: 988},
				},
			},
		},
		minConfirmations: map[wallet.Chain]int{wallet.Ethereum: 12},
	}
	w.pollOnce(ctx)

	var reservationID string
	if err := pool.QueryRow(ctx, `SELECT id FROM treasury_address_reservations WHERE address = $1`, addr).Scan(&reservationID); err != nil {
		t.Fatalf("find reservation: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM treasury_deposits WHERE tx_reference = '0xdeposit1'`).Scan(&status); err != nil {
		t.Fatalf("find deposit: %v", err)
	}
	// tip 1000 - blockHeight 988 + 1 = 13 confirmations >= minConfirmations 12
	if status != "confirmed" {
		t.Fatalf("expected status confirmed (13 confirmations >= 12 required), got %s", status)
	}
}

func TestWatcher_PollOnce_DetectedBelowThreshold(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	storeWithSeed(t, s)
	ctx := context.Background()

	_, corridorID := setupCorridor(t, pool)
	addr, err := s.allocateOrReuseAddress(ctx, wallet.Ethereum)
	if err != nil {
		t.Fatalf("allocate address: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO treasury_address_reservations
		   (corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, 'self_custody_wallet', 'self_custody', 'ETH', 'ethereum', $2, 'reserved')`,
		corridorID, addr,
	)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}

	w := &Watcher{
		store:        s,
		pollInterval: time.Second,
		clients: map[wallet.Chain]chainWatcherClient{
			wallet.Ethereum: &fakeChainWatcherClient{
				tip: 1000,
				txs: []ChainTransaction{
					{TxID: "0xdeposit2", CryptoAsset: "ETH", Amount: decimal.New(2, 0), Confirmed: true, BlockHeight: 995},
				},
			},
		},
		minConfirmations: map[wallet.Chain]int{wallet.Ethereum: 12},
	}
	w.pollOnce(ctx)

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM treasury_deposits WHERE tx_reference = '0xdeposit2'`).Scan(&status); err != nil {
		t.Fatalf("find deposit: %v", err)
	}
	// tip 1000 - blockHeight 995 + 1 = 6 confirmations < minConfirmations 12
	if status != "detected" {
		t.Fatalf("expected status detected (6 confirmations < 12 required), got %s", status)
	}
}
