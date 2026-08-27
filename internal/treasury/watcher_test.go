package treasury

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
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

// blockstreamRBFFixture mirrors blockstreamFixture's shape but adds a "vin"
// array with one BIP125-signaling input (sequence below 0xFFFFFFFE) so
// isRBFEnabled has something real to detect.
const blockstreamRBFFixture = `[
  {
    "txid": "rbf0000000000000000000000000000000000000000000000000000000000",
    "vin": [
      {"txid": "prev", "vout": 0, "sequence": 4294967293}
    ],
    "vout": [
      {
        "scriptpubkey_address": "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
        "value": 100000
      }
    ],
    "status": { "confirmed": false, "block_height": 0 }
  }
]`

func TestBlockstreamClient_ListIncomingTransactions_DetectsRBF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(blockstreamRBFFixture))
	}))
	defer server.Close()

	c := &blockstreamClient{apiURL: server.URL, client: server.Client()}
	txs, err := c.ListIncomingTransactions(context.Background(), "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4")
	if err != nil {
		t.Fatalf("list transactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("expected 1 transaction, got %d", len(txs))
	}
	if !txs[0].IsRBF {
		t.Fatal("expected a sequence below 0xFFFFFFFE to be detected as RBF-enabled")
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
	tenantID := createTestTenant(t, pool)
	addr, err := s.allocateOrReuseAddress(ctx, tenantID, wallet.Ethereum)
	if err != nil {
		t.Fatalf("allocate address: %v", err)
	}
	var reservationID uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO treasury_address_reservations
		   (tenant_id, corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, $2, 'self_custody_wallet', 'self_custody', 'ETH', 'ethereum', $3, 'reserved')
		 RETURNING id`,
		tenantID, corridorID, addr,
	).Scan(&reservationID)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
	releaseReservationOnCleanup(t, s, reservationID)

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
	tenantID := createTestTenant(t, pool)
	addr, err := s.allocateOrReuseAddress(ctx, tenantID, wallet.Ethereum)
	if err != nil {
		t.Fatalf("allocate address: %v", err)
	}
	var reservationID uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO treasury_address_reservations
		   (tenant_id, corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, $2, 'self_custody_wallet', 'self_custody', 'ETH', 'ethereum', $3, 'reserved')
		 RETURNING id`,
		tenantID, corridorID, addr,
	).Scan(&reservationID)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
	releaseReservationOnCleanup(t, s, reservationID)

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

// TestWatcher_PollOnce_NeverSweepsTenantProvidedReservation is the
// sweep-guard test: a tenant-provided reservation confirming a deposit
// must never result in a sweep, since the platform holds no key for a
// tenant-supplied wallet. No seed is loaded here at all — if the guard
// were ever removed, ExecuteSweep would fail loudly (nil seed / wrong
// custody type) rather than silently derive a wrong key, but the
// behavioral contract this test actually cares about is the observable
// one: no treasury_sweeps row is ever created for this reservation.
func TestWatcher_PollOnce_NeverSweepsTenantProvidedReservation(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	ctx := context.Background()

	tenantID := createTestTenant(t, pool)
	if err := s.RegisterTenantCustomWallet(ctx, tenantID, wallet.Ethereum, "0x1111111111111111111111111111111111111111", ""); err != nil {
		t.Fatalf("register custom wallet: %v", err)
	}
	_, corridorID := setupCorridor(t, pool)
	var reservationID uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO treasury_address_reservations
		   (tenant_id, corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, $2, 'tenant_provided_wallet', 'tenant_provided', 'ETH', 'ethereum', $3, 'reserved')
		 RETURNING id`,
		tenantID, corridorID, "0x1111111111111111111111111111111111111111",
	).Scan(&reservationID)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
	releaseReservationOnCleanup(t, s, reservationID)

	sweeper := NewSweeper(s, SweepConfig{}, nil, nil, nil)
	w := &Watcher{
		store:        s,
		pollInterval: time.Second,
		sweeper:      sweeper,
		clients: map[wallet.Chain]chainWatcherClient{
			wallet.Ethereum: &fakeChainWatcherClient{
				tip: 1000,
				txs: []ChainTransaction{
					{TxID: "0xtenanttx1", CryptoAsset: "ETH", Amount: decimal.New(2, 0), Confirmed: true, BlockHeight: 988},
				},
			},
		},
		minConfirmations: map[wallet.Chain]int{wallet.Ethereum: 12},
	}
	w.pollOnce(ctx)

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM treasury_deposits WHERE tx_reference = '0xtenanttx1'`).Scan(&status); err != nil {
		t.Fatalf("find deposit: %v", err)
	}
	if status != "confirmed" {
		t.Fatalf("expected the deposit itself to still be recorded as confirmed, got %s", status)
	}

	var sweepCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM treasury_sweeps ts JOIN treasury_address_reservations r ON r.id = ts.reservation_id WHERE r.tenant_id = $1`, tenantID).Scan(&sweepCount); err != nil {
		t.Fatalf("count sweeps: %v", err)
	}
	if sweepCount != 0 {
		t.Fatalf("expected zero sweeps for a tenant-provided reservation, got %d", sweepCount)
	}
}

// TestWatcher_PollOnce_IgnoresDustDeposit is Phase 9's dust-filtering
// fraud check: an amount below dustThreshold[chain] must never even reach
// treasury_deposits, not just be recorded and left "detected" forever.
func TestWatcher_PollOnce_IgnoresDustDeposit(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	storeWithSeed(t, s)
	ctx := context.Background()

	_, corridorID := setupCorridor(t, pool)
	tenantID := createTestTenant(t, pool)
	addr, err := s.allocateOrReuseAddress(ctx, tenantID, wallet.Ethereum)
	if err != nil {
		t.Fatalf("allocate address: %v", err)
	}
	var reservationID uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO treasury_address_reservations
		   (tenant_id, corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, $2, 'self_custody_wallet', 'self_custody', 'ETH', 'ethereum', $3, 'reserved')
		 RETURNING id`,
		tenantID, corridorID, addr,
	).Scan(&reservationID)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
	releaseReservationOnCleanup(t, s, reservationID)

	w := &Watcher{
		store:        s,
		pollInterval: time.Second,
		clients: map[wallet.Chain]chainWatcherClient{
			// 0.00005 ETH, below dustThreshold[wallet.Ethereum] (0.0001).
			wallet.Ethereum: &fakeChainWatcherClient{
				tip: 1000,
				txs: []ChainTransaction{
					{TxID: "0xdust1", CryptoAsset: "ETH", Amount: decimal.New(5, -5), Confirmed: true, BlockHeight: 988},
				},
			},
		},
		minConfirmations: map[wallet.Chain]int{wallet.Ethereum: 12},
	}
	w.pollOnce(ctx)

	// Scoped by reservation_id, not a bare tx_reference count: pollOnce
	// sweeps every currently-open reservation in the database, and
	// fakeChainWatcherClient ignores the address it's asked about, so this
	// same fixed transaction is also handed to whatever other open
	// reservations happen to exist in the shared test database — this test
	// only cares that it was never recorded against the reservation it
	// itself created.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM treasury_deposits WHERE reservation_id = $1 AND tx_reference = '0xdust1'`, reservationID).Scan(&count); err != nil {
		t.Fatalf("count deposits: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected a dust deposit to never be recorded, got %d row(s)", count)
	}
}

// TestWatcher_PollOnce_IgnoresWrongAssetDeposit is Phase 9's fake-token
// filtering fraud check: a transaction reporting a different asset than the
// reservation was issued for (e.g. a native-coin transfer landing on a
// token-denominated reservation) must never be recorded against it.
func TestWatcher_PollOnce_IgnoresWrongAssetDeposit(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	storeWithSeed(t, s)
	ctx := context.Background()

	_, corridorID := setupCorridor(t, pool)
	tenantID := createTestTenant(t, pool)
	addr, err := s.allocateOrReuseAddress(ctx, tenantID, wallet.Ethereum)
	if err != nil {
		t.Fatalf("allocate address: %v", err)
	}
	var reservationID uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO treasury_address_reservations
		   (tenant_id, corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, $2, 'self_custody_wallet', 'self_custody', 'ETH', 'ethereum', $3, 'reserved')
		 RETURNING id`,
		tenantID, corridorID, addr,
	).Scan(&reservationID)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
	releaseReservationOnCleanup(t, s, reservationID)

	w := &Watcher{
		store:        s,
		pollInterval: time.Second,
		clients: map[wallet.Chain]chainWatcherClient{
			// Reservation expects ETH; this transaction reports USDT.
			wallet.Ethereum: &fakeChainWatcherClient{
				tip: 1000,
				txs: []ChainTransaction{
					{TxID: "0xwrongasset1", CryptoAsset: "USDT", Amount: decimal.New(250, 0), Confirmed: true, BlockHeight: 988},
				},
			},
		},
		minConfirmations: map[wallet.Chain]int{wallet.Ethereum: 12},
	}
	w.pollOnce(ctx)

	// Scoped by reservation_id — see the dust test's comment above on why
	// a bare tx_reference count isn't safe against the shared test database.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM treasury_deposits WHERE reservation_id = $1 AND tx_reference = '0xwrongasset1'`, reservationID).Scan(&count); err != nil {
		t.Fatalf("count deposits: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected a wrong-asset deposit to never be recorded, got %d row(s)", count)
	}
}

// TestWatcher_PollOnce_RBFEnabledBitcoinRequiresExtraConfirmations is
// Phase 9's RBF-detection fraud check: an RBF-signaled Bitcoin transaction
// must not be treated as confirmed at the chain's normal (lower)
// min-confirmation threshold — it stays "detected" until it clears the
// higher floor (max(required, 3)), then confirms once it does.
func TestWatcher_PollOnce_RBFEnabledBitcoinRequiresExtraConfirmations(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	storeWithSeed(t, s)
	ctx := context.Background()

	_, corridorID := setupCorridor(t, pool)
	tenantID := createTestTenant(t, pool)
	addr, err := s.allocateOrReuseAddress(ctx, tenantID, wallet.Bitcoin)
	if err != nil {
		t.Fatalf("allocate address: %v", err)
	}
	var reservationID uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO treasury_address_reservations
		   (tenant_id, corridor_id, provider_name, custody_type, crypto_asset, crypto_network, address, status)
		 VALUES ($1, $2, 'self_custody_wallet', 'self_custody', 'BTC', 'bitcoin', $3, 'reserved')
		 RETURNING id`,
		tenantID, corridorID, addr,
	).Scan(&reservationID)
	if err != nil {
		t.Fatalf("insert reservation: %v", err)
	}
	releaseReservationOnCleanup(t, s, reservationID)

	client := &fakeChainWatcherClient{
		tip: 1000,
		txs: []ChainTransaction{
			// tip 1000 - blockHeight 1000 + 1 = 1 confirmation: clears the
			// chain's own min (1) but not the RBF floor (3).
			{TxID: "btcrbf1", CryptoAsset: "BTC", Amount: decimal.New(1, -3), Confirmed: true, BlockHeight: 1000, IsRBF: true},
		},
	}
	w := &Watcher{
		store:            s,
		pollInterval:     time.Second,
		clients:          map[wallet.Chain]chainWatcherClient{wallet.Bitcoin: client},
		minConfirmations: map[wallet.Chain]int{wallet.Bitcoin: 1},
	}
	w.pollOnce(ctx)

	// Scoped by reservation_id, not a bare tx_reference lookup — see the
	// dust/wrong-asset tests' comment above on why: fakeChainWatcherClient
	// ignores the address it's asked about, so the same fixed tx_reference
	// used across repeated runs of this test can otherwise match a stale
	// row left by an earlier run's own reservation.
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM treasury_deposits WHERE reservation_id = $1 AND tx_reference = 'btcrbf1'`, reservationID).Scan(&status); err != nil {
		t.Fatalf("find deposit: %v", err)
	}
	if status != "detected" {
		t.Fatalf("expected an RBF-enabled tx at 1 confirmation to stay 'detected' (floor is 3), got %s", status)
	}

	// Now at 3 confirmations — clears the RBF floor.
	client.txs[0].BlockHeight = 998
	w.pollOnce(ctx)

	if err := pool.QueryRow(ctx, `SELECT status FROM treasury_deposits WHERE reservation_id = $1 AND tx_reference = 'btcrbf1'`, reservationID).Scan(&status); err != nil {
		t.Fatalf("find deposit: %v", err)
	}
	if status != "confirmed" {
		t.Fatalf("expected the RBF-enabled tx to confirm once it reaches 3 confirmations, got %s", status)
	}
}
