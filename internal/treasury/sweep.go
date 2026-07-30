package treasury

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

var ErrNoSweepDestination = errors.New("treasury: no sweep destination configured for this chain/asset")

// isStableAsset classifies USDT variants as "stable" for sweep-batching
// purposes — a v2-only refinement (see ARCHITECTURE.md §3), confirmed
// absent from v1, which sweeps everything immediately with no batching.
// Everything else on the launch list (BTC/ETH/BNB/TRX) is volatile and
// sweeps immediately on confirmation.
func isStableAsset(cryptoAsset string) bool {
	return strings.HasPrefix(strings.ToUpper(cryptoAsset), "USDT")
}

// gasTopUpAmount is the native-asset balance a token-holding deposit
// address is topped up to (from the gas-funding wallet — a separate HD
// account, see wallet.Seed.DeriveGasFundingPrivateKey) before a token
// sweep, if its balance is below this. A fixed target rather than a
// precise gas estimate — simpler, and the small overpayment is immaterial
// next to what's being swept.
var gasTopUpAmount = map[wallet.Chain]decimal.Decimal{
	wallet.Ethereum: decimal.NewFromFloat(0.005),
	wallet.BSC:      decimal.NewFromFloat(0.01),
	wallet.Tron:     decimal.NewFromInt(20),
}

// Sweeper executes sweeps: immediate for volatile assets (called inline
// from the watcher on confirmation — see watcher.go), batched for stable
// assets (via RunBatchSweep below). Signing uses the same wallet package
// primitives already verified in wallet's own test suite (BTC via
// txscript's real execution engine, EVM signatures via recovery-based
// self-verification); this orchestration layer's live broadcast paths have
// not been exercised against real funded addresses (see the plan's
// verification notes) — building/signing logic is real, not stubbed.
type Sweeper struct {
	store                  *Store
	httpClient             *http.Client
	stableBalanceThreshold decimal.Decimal
	stableTimeBackstop     time.Duration

	blockstream *blockstreamClient
	ethereum    *etherscanClient
	bsc         *etherscanClient
	tronRest    *tronGridWatcherClient
	tronClient  *wallet.TronClient // gRPC — signing/broadcast only
}

// NewSweeper builds a Sweeper. tronClient must already be Connect()-ed if
// Tron is enabled — main.go's responsibility, same as any other
// long-lived connection this codebase manages explicitly.
func NewSweeper(store *Store, cfg SweepConfig, chains map[wallet.Chain]ChainConfig, tronClient *wallet.TronClient, httpClient *http.Client) *Sweeper {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	s := &Sweeper{
		store:                  store,
		httpClient:             httpClient,
		stableBalanceThreshold: cfg.StableBalanceThreshold,
		stableTimeBackstop:     cfg.StableTimeBackstop,
		tronClient:             tronClient,
	}
	if cc, ok := chains[wallet.Bitcoin]; ok && cc.Enabled {
		s.blockstream = &blockstreamClient{apiURL: cc.APIURL, client: httpClient}
	}
	if cc, ok := chains[wallet.Ethereum]; ok && cc.Enabled {
		s.ethereum = &etherscanClient{apiURL: cc.APIURL, apiKey: cc.APIKey, chain: wallet.Ethereum, client: httpClient}
	}
	if cc, ok := chains[wallet.BSC]; ok && cc.Enabled {
		s.bsc = &etherscanClient{apiURL: cc.APIURL, apiKey: cc.APIKey, chain: wallet.BSC, client: httpClient}
	}
	if cc, ok := chains[wallet.Tron]; ok && cc.Enabled {
		s.tronRest = &tronGridWatcherClient{apiURL: cc.APIURL, apiKey: cc.APIKey, client: httpClient}
	}
	return s
}

// sweepPendingSentinel signals ExecuteSweep sent a gas top-up and deferred
// the actual token sweep to a later call — not a failure, just "not yet."
var errSweepDeferredForGas = errors.New("treasury: gas top-up sent, sweep deferred to next cycle")

// ExecuteSweep sweeps every confirmed, not-yet-swept deposit for
// reservationID to its configured destination. Safe to call repeatedly —
// getSweepableDeposits only ever returns unswept deposits, so a call with
// nothing to do is a no-op, not an error.
func (s *Sweeper) ExecuteSweep(ctx context.Context, reservationID uuid.UUID) error {
	r, err := s.store.GetReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if r.CustodyType != CustodyTypeSelf {
		return fmt.Errorf("treasury: sweep: reservation %s is not self-custody", reservationID)
	}
	if s.store.seed == nil {
		return ErrHDWalletNotInitialized
	}
	chain := wallet.Chain(r.CryptoNetwork)

	deposits, total, err := s.store.getSweepableDeposits(ctx, reservationID)
	if err != nil {
		return err
	}
	if len(deposits) == 0 {
		return nil
	}

	destination, err := s.store.getSweepDestination(ctx, r.CryptoNetwork, r.CryptoAsset)
	if err != nil {
		return err
	}

	_, index, err := s.store.findDerivationIndex(ctx, chain, r.Address)
	if err != nil {
		return err
	}
	priv, err := s.store.seed.DerivePrivateKey(chain, index)
	if err != nil {
		return fmt.Errorf("treasury: sweep: derive key: %w", err)
	}

	sweepID, err := s.store.persistSweep(ctx, reservationID, r.CryptoAsset, total, destination)
	if err != nil {
		return err
	}

	var txHash string
	switch chain {
	case wallet.Bitcoin:
		txHash, err = s.sweepBTC(ctx, priv, r.Address, destination)
	case wallet.Ethereum:
		txHash, err = s.sweepEVM(ctx, s.ethereum, wallet.Ethereum, priv, r.Address, r.CryptoAsset, total, destination)
	case wallet.BSC:
		txHash, err = s.sweepEVM(ctx, s.bsc, wallet.BSC, priv, r.Address, r.CryptoAsset, total, destination)
	case wallet.Tron:
		txHash, err = s.sweepTron(ctx, priv, r.Address, r.CryptoAsset, total, destination)
	default:
		err = fmt.Errorf("treasury: sweep: unsupported chain %q", chain)
	}

	if errors.Is(err, errSweepDeferredForGas) {
		_ = s.store.updateSweepStatus(ctx, sweepID, "pending", "")
		return nil // retried next cycle once the top-up has confirmed
	}
	if err != nil {
		_ = s.store.updateSweepStatus(ctx, sweepID, "failed", "")
		return fmt.Errorf("treasury: sweep %s: %w", chain, err)
	}

	if err := s.store.updateSweepStatus(ctx, sweepID, "broadcast", txHash); err != nil {
		return err
	}
	depositIDs := make([]uuid.UUID, len(deposits))
	for i, d := range deposits {
		depositIDs[i] = d.ID
	}
	return s.store.markDepositsSwept(ctx, depositIDs, sweepID)
}

func (s *Sweeper) sweepBTC(ctx context.Context, priv *btcec.PrivateKey, address, destination string) (string, error) {
	if s.blockstream == nil {
		return "", fmt.Errorf("treasury: bitcoin watcher not configured")
	}
	utxos, err := s.blockstream.ListUTXOs(ctx, address)
	if err != nil {
		return "", err
	}
	if len(utxos) == 0 {
		return "", fmt.Errorf("treasury: no confirmed utxos at %s", address)
	}
	const flatFeeSats = 2000 // simplification — not a live fee-rate estimate
	rawTxHex, _, err := wallet.BuildAndSignBTCSweep(priv, utxos, destination, flatFeeSats)
	if err != nil {
		return "", err
	}
	return wallet.BroadcastBTCTransaction(ctx, s.httpClient, s.blockstream.apiURL, rawTxHex)
}

func (s *Sweeper) sweepEVM(ctx context.Context, client *etherscanClient, chain wallet.Chain, priv *btcec.PrivateKey, address, cryptoAsset string, amount decimal.Decimal, destination string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("treasury: %s watcher not configured", chain)
	}

	if isStableAsset(cryptoAsset) {
		balance, err := client.NativeBalance(ctx, address)
		if err != nil {
			return "", err
		}
		if balance.LessThan(gasTopUpAmount[chain]) {
			if err := s.topUpGasEVM(ctx, client, chain, address, gasTopUpAmount[chain].Sub(balance)); err != nil {
				return "", fmt.Errorf("treasury: gas top-up: %w", err)
			}
			return "", errSweepDeferredForGas
		}
		return s.broadcastEVMTokenTransfer(ctx, client, chain, priv, destination, amount)
	}

	return s.broadcastEVMNativeTransfer(ctx, client, chain, priv, destination, amount)
}

func (s *Sweeper) topUpGasEVM(ctx context.Context, client *etherscanClient, chain wallet.Chain, toAddress string, amount decimal.Decimal) error {
	gasKey, err := s.store.seed.DeriveGasFundingPrivateKey(chain)
	if err != nil {
		return err
	}
	_, err = s.broadcastEVMNativeTransfer(ctx, client, chain, gasKey, toAddress, amount)
	return err
}

func (s *Sweeper) broadcastEVMNativeTransfer(ctx context.Context, client *etherscanClient, chain wallet.Chain, priv *btcec.PrivateKey, toAddress string, amount decimal.Decimal) (string, error) {
	fromAddr, err := wallet.DeriveAddressFromKey(priv, chain)
	if err != nil {
		return "", err
	}
	nonce, err := client.NonceAt(ctx, fromAddr)
	if err != nil {
		return "", err
	}
	gasPrice, err := client.SuggestedGasPrice(ctx)
	if err != nil {
		return "", err
	}
	var to [20]byte
	if err := hexAddressInto(toAddress, &to); err != nil {
		return "", err
	}
	weiAmount := amount.Shift(18).BigInt()

	rawTx, _, err := wallet.BuildAndSignEVMTx(priv, wallet.EVMTxParams{
		ChainID:              uint64(etherscanChainID[chain]),
		Nonce:                nonce,
		MaxPriorityFeePerGas: gasPrice.BigInt(),
		MaxFeePerGas:         gasPrice.BigInt(),
		GasLimit:             21000,
		To:                   to,
		Value:                weiAmount,
	})
	if err != nil {
		return "", err
	}
	return wallet.BroadcastEVMTransaction(ctx, s.httpClient, client.baseURL(), rawTx)
}

func (s *Sweeper) broadcastEVMTokenTransfer(ctx context.Context, client *etherscanClient, chain wallet.Chain, priv *btcec.PrivateKey, toAddress string, amount decimal.Decimal) (string, error) {
	fromAddr, err := wallet.DeriveAddressFromKey(priv, chain)
	if err != nil {
		return "", err
	}
	nonce, err := client.NonceAt(ctx, fromAddr)
	if err != nil {
		return "", err
	}
	gasPrice, err := client.SuggestedGasPrice(ctx)
	if err != nil {
		return "", err
	}
	var to [20]byte
	if err := hexAddressInto(toAddress, &to); err != nil {
		return "", err
	}
	contract, ok := usdtContractAddress[chain]
	if !ok {
		return "", fmt.Errorf("treasury: no usdt contract configured for %s", chain)
	}
	var contractAddr [20]byte
	if err := hexAddressInto(contract, &contractAddr); err != nil {
		return "", err
	}
	tokenAmount := amount.Shift(6).BigInt() // USDT is 6 decimals on every launch chain

	rawTx, _, err := wallet.BuildAndSignEVMTx(priv, wallet.EVMTxParams{
		ChainID:              uint64(etherscanChainID[chain]),
		Nonce:                nonce,
		MaxPriorityFeePerGas: gasPrice.BigInt(),
		MaxFeePerGas:         gasPrice.BigInt(),
		GasLimit:             80000, // ERC20 transfer() headroom
		To:                   contractAddr,
		Value:                big.NewInt(0),
		Data:                 wallet.BuildERC20TransferData(to, tokenAmount),
	})
	if err != nil {
		return "", err
	}
	return wallet.BroadcastEVMTransaction(ctx, s.httpClient, client.baseURL(), rawTx)
}

func (s *Sweeper) sweepTron(ctx context.Context, priv *btcec.PrivateKey, address, cryptoAsset string, amount decimal.Decimal, destination string) (string, error) {
	if s.tronClient == nil || s.tronRest == nil {
		return "", fmt.Errorf("treasury: tron watcher/client not configured")
	}

	if isStableAsset(cryptoAsset) {
		balance, err := s.tronRest.NativeBalance(ctx, address)
		if err != nil {
			return "", err
		}
		if balance.LessThan(gasTopUpAmount[wallet.Tron]) {
			gasKey, err := s.store.seed.DeriveGasFundingPrivateKey(wallet.Tron)
			if err != nil {
				return "", err
			}
			topUp := gasTopUpAmount[wallet.Tron].Sub(balance)
			if _, err := s.tronClient.SweepTRX(ctx, gasKey, address, topUp.Shift(6).IntPart()); err != nil {
				return "", fmt.Errorf("treasury: gas top-up: %w", err)
			}
			return "", errSweepDeferredForGas
		}
		contract := usdtContractAddress[wallet.Tron]
		const feeLimitSun = 100_000_000 // 100 TRX — plain-fee fallback, see TronClient's doc comment
		return s.tronClient.SweepTRC20(ctx, priv, destination, contract, amount.Shift(6).BigInt(), feeLimitSun)
	}

	return s.tronClient.SweepTRX(ctx, priv, destination, amount.Shift(6).IntPart())
}

// RunBatchSweep polls on pollInterval until ctx is canceled, sweeping every
// self-custody reservation holding a stable-asset balance that has
// crossed the configured threshold or time backstop. Volatile assets never
// go through here — they sweep immediately, triggered inline from the
// watcher (see watcher.go) as soon as a deposit confirms.
func (s *Sweeper) RunBatchSweep(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.batchSweepOnce(ctx)
		}
	}
}

func (s *Sweeper) batchSweepOnce(ctx context.Context) {
	candidates, err := s.store.listStableSweepCandidates(ctx, s.stableBalanceThreshold, s.stableTimeBackstop)
	if err != nil {
		log.Printf("treasury: batch sweep: list candidates: %v", err)
		return
	}
	for _, reservationID := range candidates {
		if err := s.ExecuteSweep(ctx, reservationID); err != nil {
			log.Printf("treasury: batch sweep: reservation %s: %v", reservationID, err)
		}
	}
}

// --- Store-level bookkeeping ---

type sweepableDeposit struct {
	ID uuid.UUID
}

func (s *Store) getSweepableDeposits(ctx context.Context, reservationID uuid.UUID) ([]sweepableDeposit, decimal.Decimal, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, amount FROM treasury_deposits
		 WHERE reservation_id = $1 AND status = 'confirmed' AND swept_at IS NULL`,
		reservationID,
	)
	if err != nil {
		return nil, decimal.Decimal{}, fmt.Errorf("treasury: get sweepable deposits: %w", err)
	}
	defer rows.Close()

	var deposits []sweepableDeposit
	total := decimal.Zero
	for rows.Next() {
		var d sweepableDeposit
		var amount decimal.Decimal
		if err := rows.Scan(&d.ID, &amount); err != nil {
			return nil, decimal.Decimal{}, fmt.Errorf("treasury: scan sweepable deposit: %w", err)
		}
		total = total.Add(amount)
		deposits = append(deposits, d)
	}
	if err := rows.Err(); err != nil {
		return nil, decimal.Decimal{}, fmt.Errorf("treasury: get sweepable deposits: %w", err)
	}
	return deposits, total, nil
}

func (s *Store) markDepositsSwept(ctx context.Context, depositIDs []uuid.UUID, sweepID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE treasury_deposits SET swept_at = now(), updated_at = now() WHERE id = ANY($1)`,
		depositIDs,
	)
	if err != nil {
		return fmt.Errorf("treasury: mark deposits swept: %w", err)
	}
	_ = sweepID
	return nil
}

func (s *Store) getSweepDestination(ctx context.Context, chain, cryptoAsset string) (string, error) {
	var addr string
	err := s.pool.QueryRow(ctx,
		`SELECT destination_address FROM sweep_destinations WHERE chain = $1 AND crypto_asset = $2`,
		chain, cryptoAsset,
	).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoSweepDestination
	}
	if err != nil {
		return "", fmt.Errorf("treasury: get sweep destination: %w", err)
	}
	return addr, nil
}

// SetSweepDestination upserts the ops-configured destination address for
// chain/cryptoAsset — the config-driven mechanism for pointing sweeps
// somewhere real, same pattern corridor.UpsertProviderBinding follows.
func (s *Store) SetSweepDestination(ctx context.Context, chain, cryptoAsset, destinationAddress string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sweep_destinations (chain, crypto_asset, destination_address)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (chain, crypto_asset) DO UPDATE SET
		   destination_address = EXCLUDED.destination_address,
		   updated_at = now()`,
		chain, cryptoAsset, destinationAddress,
	)
	if err != nil {
		return fmt.Errorf("treasury: set sweep destination: %w", err)
	}
	return nil
}

func (s *Store) persistSweep(ctx context.Context, reservationID uuid.UUID, cryptoAsset string, amount decimal.Decimal, destination string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO treasury_sweeps (reservation_id, crypto_asset, amount, destination_address, status)
		 VALUES ($1, $2, $3, $4, 'pending')
		 RETURNING id`,
		reservationID, cryptoAsset, amount, destination,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("treasury: persist sweep: %w", err)
	}
	return id, nil
}

func (s *Store) updateSweepStatus(ctx context.Context, sweepID uuid.UUID, status, txHash string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE treasury_sweeps SET status = $2, tx_hash = NULLIF($3, ''), updated_at = now() WHERE id = $1`,
		sweepID, status, txHash,
	)
	if err != nil {
		return fmt.Errorf("treasury: update sweep status: %w", err)
	}
	return nil
}

func (s *Store) findDerivationIndex(ctx context.Context, chain wallet.Chain, address string) (wallet.Chain, uint32, error) {
	var index uint32
	err := s.pool.QueryRow(ctx,
		`SELECT derivation_index FROM derived_addresses WHERE chain = $1 AND address = $2`,
		string(chain), address,
	).Scan(&index)
	if errors.Is(err, pgx.ErrNoRows) {
		return chain, 0, fmt.Errorf("treasury: no derived_addresses row for %s %s", chain, address)
	}
	if err != nil {
		return chain, 0, fmt.Errorf("treasury: find derivation index: %w", err)
	}
	return chain, index, nil
}

// listStableSweepCandidates returns self-custody reservations whose
// unswept confirmed stable-asset balance has crossed balanceThreshold or
// whose oldest unswept confirmed deposit is older than timeBackstop.
func (s *Store) listStableSweepCandidates(ctx context.Context, balanceThreshold decimal.Decimal, timeBackstop time.Duration) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, SUM(d.amount) AS total, MIN(d.confirmed_at) AS oldest
		 FROM treasury_deposits d
		 JOIN treasury_address_reservations r ON r.id = d.reservation_id
		 WHERE d.status = 'confirmed' AND d.swept_at IS NULL
		   AND r.custody_type = 'self_custody'
		   AND d.crypto_asset ILIKE 'USDT%'
		 GROUP BY r.id
		 HAVING SUM(d.amount) >= $1 OR MIN(d.confirmed_at) <= $2`,
		balanceThreshold, time.Now().Add(-timeBackstop),
	)
	if err != nil {
		return nil, fmt.Errorf("treasury: list stable sweep candidates: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var total decimal.Decimal
		var oldest *time.Time
		if err := rows.Scan(&id, &total, &oldest); err != nil {
			return nil, fmt.Errorf("treasury: scan sweep candidate: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("treasury: list stable sweep candidates: %w", err)
	}
	return out, nil
}

// hexAddressInto decodes a "0x"-prefixed 20-byte hex address into dst —
// EVM addresses are ASCII-hex, unlike BTC/Tron's Base58Check.
func hexAddressInto(address string, dst *[20]byte) error {
	trimmed := strings.TrimPrefix(address, "0x")
	decoded, err := hex.DecodeString(trimmed)
	if err != nil {
		return fmt.Errorf("treasury: decode address %q: %w", address, err)
	}
	if len(decoded) != 20 {
		return fmt.Errorf("treasury: address %q is not 20 bytes", address)
	}
	copy(dst[:], decoded)
	return nil
}
