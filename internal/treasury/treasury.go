// Package treasury is the crypto-collection layer: provider adapters
// reserve a deposit address per corridor, a webhook-driven pipeline tracks
// deposit state (pending → detected → confirmed), and custody balances
// record what a partner-custodied provider reports holding on our behalf.
//
// Phase 4 step 1 only: the Busha partner-custodied adapter (see
// ARCHITECTURE.md §2/§3). Self-custody HD wallets — key derivation, chain
// watchers, sweep policy — are a separate future step and not built here;
// no sweep-execution table or self-custody adapter exists yet.
package treasury

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

var (
	ErrNoProviderAvailable = errors.New("treasury: no active, enabled collection provider available for this corridor")
	ErrReservationNotFound = errors.New("treasury: reservation not found")
)

// AddressReservation is a persisted record of one deposit address handed
// out for a corridor.
type AddressReservation struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	CorridorID        uuid.UUID
	ProviderName      string
	CustodyType       CustodyType
	CryptoAsset       string
	CryptoNetwork     string
	Address           string
	AddressTag        string
	ProviderReference string
	Status            string
	ReservedAt        time.Time
	ReleasedAt        *time.Time
}

// DepositInstructions is the caller-facing shape returned by
// GetDepositInstructions — what Phase 5's session module hands to a
// depositor.
type DepositInstructions struct {
	ReservationID uuid.UUID
	TenantID      uuid.UUID
	ProviderName  string
	CryptoAsset   string
	CryptoNetwork string
	Address       string
	AddressTag    string
}

// Deposit is the webhook-driven state of one deposit against a
// reservation.
type Deposit struct {
	ID              uuid.UUID
	ReservationID   uuid.UUID
	Status          string
	CryptoAsset     string
	Amount          decimal.Decimal
	TxReference     string
	ProviderPayload []byte
	DetectedAt      *time.Time
	ConfirmedAt     *time.Time
}

// CustodyBalance is the latest snapshot a partner-custodied provider
// reports holding on our behalf for one asset.
type CustodyBalance struct {
	ProviderName string
	CryptoAsset  string
	Balance      decimal.Decimal
	AsOf         time.Time
}

// Config is what main.go builds from *config.Config to construct a Store —
// same convention rate.Config/corridor follow, taking already-extracted
// values rather than importing platform/config directly.
type Config struct {
	Busha CollectionProviderConfig

	Bitcoin  ChainConfig
	Ethereum ChainConfig
	BSC      ChainConfig
	Tron     ChainConfig

	Watcher WatcherConfig
	Sweep   SweepConfig
}

// ChainConfig mirrors config.ChainConfig — see watcher.go.
type ChainConfig struct {
	Enabled          bool
	APIURL           string
	APIKey           string
	MinConfirmations int
}

// WatcherConfig mirrors config.WatcherConfig — see watcher.go.
type WatcherConfig struct {
	PollInterval time.Duration
}

// SweepConfig mirrors config.SweepConfig — see sweep.go.
type SweepConfig struct {
	StableBalanceThreshold decimal.Decimal
	StableTimeBackstop     time.Duration
}

type Store struct {
	pool               *db.Pool
	corridorStore      *corridor.Store
	providers          map[string]CollectionProvider
	bushaWebhookSecret string

	// seed is the decrypted HD wallet seed, populated by LoadHDWalletSeed.
	// nil until then — the self-custody provider stays disabled (see
	// selfCustodyProvider.IsEnabled) until a seed has been loaded.
	seed *wallet.Seed
}

func New(pool *db.Pool, corridorStore *corridor.Store, cfg Config) *Store {
	busha := newBushaProvider(cfg.Busha)
	s := &Store{
		pool:               pool,
		corridorStore:      corridorStore,
		bushaWebhookSecret: cfg.Busha.WebhookSecret,
	}
	selfCustody := &selfCustodyProvider{store: s}
	s.providers = map[string]CollectionProvider{
		busha.Name():       busha,
		selfCustody.Name(): selfCustody,
	}
	return s
}

// ReserveAddress picks the corridor's active collection-provider bindings
// (corridor.ListActiveProviders, priority order) and reserves an address
// from the first one with a registered, enabled adapter — failing over to
// the next binding on error, same failover shape corridor's provider
// bindings already establish for other provider types. tenantID is whose
// deposit this reservation is for — see CollectionProvider's doc comment.
func (s *Store) ReserveAddress(ctx context.Context, tenantID, corridorID uuid.UUID) (*AddressReservation, error) {
	corr, err := s.corridorStore.GetCorridorByID(ctx, corridorID)
	if err != nil {
		return nil, fmt.Errorf("treasury: reserve address: %w", err)
	}

	bindings, err := s.corridorStore.ListActiveProviders(ctx, corridorID, corridor.ProviderTypeCollection)
	if err != nil {
		return nil, fmt.Errorf("treasury: reserve address: %w", err)
	}

	var lastErr error
	for _, b := range bindings {
		provider, ok := s.providers[b.ProviderName]
		if !ok || !provider.IsEnabled() {
			continue
		}
		addr, err := provider.ReserveAddress(ctx, tenantID, corr.CryptoAsset, corr.CryptoNetwork)
		if err != nil {
			lastErr = err
			continue
		}
		return s.persistReservation(ctx, tenantID, corridorID, provider, corr.CryptoAsset, corr.CryptoNetwork, addr)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w: last error: %v", ErrNoProviderAvailable, lastErr)
	}
	return nil, ErrNoProviderAvailable
}

func (s *Store) persistReservation(ctx context.Context, tenantID, corridorID uuid.UUID, provider CollectionProvider, cryptoAsset, cryptoNetwork string, addr ProviderAddress) (*AddressReservation, error) {
	r := &AddressReservation{
		TenantID:          tenantID,
		CorridorID:        corridorID,
		ProviderName:      provider.Name(),
		CustodyType:       provider.CustodyType(),
		CryptoAsset:       cryptoAsset,
		CryptoNetwork:     cryptoNetwork,
		Address:           addr.Address,
		AddressTag:        addr.AddressTag,
		ProviderReference: addr.ProviderReference,
		Status:            "reserved",
		ReservedAt:        time.Now(),
	}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO treasury_address_reservations
		   (tenant_id, corridor_id, provider_name, custody_type, crypto_asset, crypto_network,
		    address, address_tag, provider_reference, status, reserved_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 RETURNING id`,
		r.TenantID, r.CorridorID, r.ProviderName, string(r.CustodyType), r.CryptoAsset, r.CryptoNetwork,
		r.Address, r.AddressTag, r.ProviderReference, r.Status, r.ReservedAt,
	).Scan(&r.ID)
	if err != nil {
		return nil, fmt.Errorf("treasury: persist reservation: %w", err)
	}
	return r, nil
}

// GetReservation looks up a previously persisted reservation by id.
func (s *Store) GetReservation(ctx context.Context, id uuid.UUID) (*AddressReservation, error) {
	var r AddressReservation
	var custodyType string
	r.ID = id
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, corridor_id, provider_name, custody_type, crypto_asset, crypto_network,
		        address, address_tag, provider_reference, status, reserved_at, released_at
		 FROM treasury_address_reservations WHERE id = $1`,
		id,
	).Scan(&r.TenantID, &r.CorridorID, &r.ProviderName, &custodyType, &r.CryptoAsset, &r.CryptoNetwork,
		&r.Address, &r.AddressTag, &r.ProviderReference, &r.Status, &r.ReservedAt, &r.ReleasedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrReservationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("treasury: get reservation: %w", err)
	}
	r.CustodyType = CustodyType(custodyType)
	return &r, nil
}

// GetDepositInstructions is the method Phase 5's session module actually
// calls: reserves an address and formats it into the caller-facing DTO a
// depositor is shown.
func (s *Store) GetDepositInstructions(ctx context.Context, tenantID, corridorID uuid.UUID) (*DepositInstructions, error) {
	r, err := s.ReserveAddress(ctx, tenantID, corridorID)
	if err != nil {
		return nil, err
	}
	return &DepositInstructions{
		ReservationID: r.ID,
		TenantID:      r.TenantID,
		ProviderName:  r.ProviderName,
		CryptoAsset:   r.CryptoAsset,
		CryptoNetwork: r.CryptoNetwork,
		Address:       r.Address,
		AddressTag:    r.AddressTag,
	}, nil
}

// RecordCustodyBalance upserts the latest balance a partner-custodied
// provider reports holding for one asset. Populated ad hoc or via webhook
// for now — no background poller exists yet, since Busha's real
// balance-check endpoint isn't known (see providers.go TODOs); a periodic
// reconciliation job is Phase 8 territory once a real spec exists.
func (s *Store) RecordCustodyBalance(ctx context.Context, providerName, cryptoAsset string, balance decimal.Decimal, asOf time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO treasury_custody_balances (provider_name, crypto_asset, balance, as_of)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (provider_name, crypto_asset) DO UPDATE SET
		   balance = EXCLUDED.balance,
		   as_of = EXCLUDED.as_of,
		   updated_at = now()`,
		providerName, cryptoAsset, balance, asOf,
	)
	if err != nil {
		return fmt.Errorf("treasury: record custody balance: %w", err)
	}
	return nil
}

// GetCustodyBalance returns the latest recorded balance for a
// provider/asset pair.
func (s *Store) GetCustodyBalance(ctx context.Context, providerName, cryptoAsset string) (*CustodyBalance, error) {
	b := &CustodyBalance{ProviderName: providerName, CryptoAsset: cryptoAsset}
	err := s.pool.QueryRow(ctx,
		`SELECT balance, as_of FROM treasury_custody_balances WHERE provider_name = $1 AND crypto_asset = $2`,
		providerName, cryptoAsset,
	).Scan(&b.Balance, &b.AsOf)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("treasury: no custody balance recorded for %s/%s", providerName, cryptoAsset)
	}
	if err != nil {
		return nil, fmt.Errorf("treasury: get custody balance: %w", err)
	}
	return b, nil
}
