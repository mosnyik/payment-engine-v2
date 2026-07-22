package treasury

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sirfi/payment-engine-v2/internal/platform/crypto"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

var (
	// ErrHDWalletAlreadyInitialized guards the hd_wallet_seed singleton —
	// InitializeHDWalletSeed is a one-time operation (adminctl -init-wallet).
	ErrHDWalletAlreadyInitialized = errors.New("treasury: hd wallet seed already initialized")
	// ErrHDWalletNotInitialized means no seed has been provisioned yet —
	// self-custody stays disabled until adminctl -init-wallet has run.
	ErrHDWalletNotInitialized = errors.New("treasury: hd wallet seed not initialized")
)

// InitializeHDWalletSeed encrypts mnemonic with encryptionKey and persists
// it as the singleton hd_wallet_seed row. One-time — called only by
// adminctl -init-wallet. Fixes the exact audit gap v1 had (ciphertext and
// key colocated in the same .env): the ciphertext lands here, in the DB;
// the key lives in config (HD_WALLET_SEED_ENCRYPTION_KEY), never together.
func (s *Store) InitializeHDWalletSeed(ctx context.Context, mnemonic string, encryptionKey []byte) error {
	aesgcm, err := crypto.NewAESGCM(encryptionKey)
	if err != nil {
		return fmt.Errorf("treasury: build seed encryptor: %w", err)
	}
	ciphertext, err := aesgcm.Encrypt(mnemonic)
	if err != nil {
		return fmt.Errorf("treasury: encrypt seed: %w", err)
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO hd_wallet_seed (id, mnemonic_ciphertext) VALUES (1, $1)`,
		ciphertext,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrHDWalletAlreadyInitialized
		}
		return fmt.Errorf("treasury: persist seed: %w", err)
	}
	return nil
}

// LoadHDWalletSeed decrypts the persisted seed with encryptionKey and
// holds it in memory for the lifetime of the Store — self-custody address
// derivation and sweep signing are disabled until this succeeds. Call once
// at startup, after config is loaded.
func (s *Store) LoadHDWalletSeed(ctx context.Context, encryptionKey []byte) error {
	var ciphertext string
	err := s.pool.QueryRow(ctx, `SELECT mnemonic_ciphertext FROM hd_wallet_seed WHERE id = 1`).Scan(&ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrHDWalletNotInitialized
	}
	if err != nil {
		return fmt.Errorf("treasury: load seed: %w", err)
	}

	aesgcm, err := crypto.NewAESGCM(encryptionKey)
	if err != nil {
		return fmt.Errorf("treasury: build seed decryptor: %w", err)
	}
	mnemonic, err := aesgcm.Decrypt(ciphertext)
	if err != nil {
		return fmt.Errorf("treasury: decrypt seed: %w", err)
	}

	seed, err := wallet.NewSeed(mnemonic)
	if err != nil {
		return fmt.Errorf("treasury: rebuild seed: %w", err)
	}
	s.seed = seed
	return nil
}

// Close releases the Store's held resources — zeroes the decrypted HD
// wallet seed, if one was loaded (see wallet.Seed.Close).
func (s *Store) Close() {
	if s.seed != nil {
		s.seed.Close()
	}
}

// getOrAllocateTenantAccount returns tenantID's assigned HD account number
// (wallet.TenantAccountOffset and up — accounts 0/1 are reserved for
// platform-level deposit/gas-funding wallets, never assigned to a tenant),
// allocating one on first use and returning the same one on every later
// call. The tenant_id primary key on treasury_tenant_hd_accounts makes a
// concurrent double-allocation for the same tenant a harmless conflict —
// the loser just re-reads the winner's row.
func (s *Store) getOrAllocateTenantAccount(ctx context.Context, tenantID uuid.UUID) (uint32, error) {
	var account uint64
	err := s.pool.QueryRow(ctx,
		`SELECT account_index FROM treasury_tenant_hd_accounts WHERE tenant_id = $1`,
		tenantID,
	).Scan(&account)
	if err == nil {
		return uint32(account), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("treasury: get tenant hd account: %w", err)
	}

	err = s.pool.QueryRow(ctx,
		`INSERT INTO treasury_hd_account_counter (id, next_account_index) VALUES (1, 3)
		 ON CONFLICT (id) DO UPDATE SET next_account_index = treasury_hd_account_counter.next_account_index + 1
		 RETURNING next_account_index - 1`,
	).Scan(&account)
	if err != nil {
		return 0, fmt.Errorf("treasury: allocate tenant hd account counter: %w", err)
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO treasury_tenant_hd_accounts (tenant_id, account_index) VALUES ($1, $2)`,
		tenantID, account,
	)
	if err != nil {
		if isUniqueViolation(err) {
			// Lost the race to a concurrent allocation for the same
			// tenant — the winner's row is authoritative, use it.
			return s.getOrAllocateTenantAccount(ctx, tenantID)
		}
		return 0, fmt.Errorf("treasury: persist tenant hd account: %w", err)
	}
	return uint32(account), nil
}

// allocateOrReuseAddress returns a self-custody deposit address for
// tenantID on chain, reusing a currently-unreserved previously-derived
// address from that tenant's own pool when one exists (ARCHITECTURE.md
// §3: "Reuse per end-user") and deriving a new HD index only when none is
// free. Safe under concurrent callers: the actual safety net is the
// partial unique index on treasury_address_reservations(address) WHERE
// status = 'reserved' (see migration 000011) — a race here degrades to a
// persistReservation error, not a silently double-booked address, which
// is the actual audit-lesson-3 fix; this function's reuse-first check is
// an optimization on top of it, not the safety mechanism itself.
func (s *Store) allocateOrReuseAddress(ctx context.Context, tenantID uuid.UUID, chain wallet.Chain) (string, error) {
	if s.seed == nil {
		return "", ErrHDWalletNotInitialized
	}

	if addr, ok, err := s.findFreeDerivedAddress(ctx, tenantID, chain); err != nil {
		return "", err
	} else if ok {
		return addr, nil
	}

	tenantAccount, err := s.getOrAllocateTenantAccount(ctx, tenantID)
	if err != nil {
		return "", err
	}
	index, err := s.allocateNextIndex(ctx, tenantID, chain)
	if err != nil {
		return "", err
	}
	addr, err := wallet.DeriveAddressAtAccount(s.seed, chain, tenantAccount, index)
	if err != nil {
		return "", fmt.Errorf("treasury: derive address for %s index %d: %w", chain, index, err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO derived_addresses (chain, tenant_id, derivation_index, address) VALUES ($1, $2, $3, $4)`,
		string(chain), tenantID, index, addr,
	)
	if err != nil {
		return "", fmt.Errorf("treasury: persist derived address: %w", err)
	}
	return addr, nil
}

func (s *Store) findFreeDerivedAddress(ctx context.Context, tenantID uuid.UUID, chain wallet.Chain) (string, bool, error) {
	var addr string
	err := s.pool.QueryRow(ctx,
		`SELECT da.address
		 FROM derived_addresses da
		 LEFT JOIN treasury_address_reservations r
		   ON r.address = da.address AND r.status = 'reserved'
		 WHERE da.chain = $1 AND da.tenant_id = $2 AND r.id IS NULL
		 ORDER BY da.derivation_index ASC
		 LIMIT 1`,
		string(chain), tenantID,
	).Scan(&addr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("treasury: find free derived address: %w", err)
	}
	return addr, true, nil
}

// allocateNextIndex atomically allocates and returns the next unused HD
// derivation index for tenantID on chain — a single INSERT ... ON
// CONFLICT statement, so no explicit row lock/transaction is needed at
// this call site.
func (s *Store) allocateNextIndex(ctx context.Context, tenantID uuid.UUID, chain wallet.Chain) (uint32, error) {
	var next uint64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO hd_wallet_indices (chain, tenant_id, next_index) VALUES ($1, $2, 1)
		 ON CONFLICT (chain, tenant_id) DO UPDATE SET
		   next_index = hd_wallet_indices.next_index + 1,
		   updated_at = now()
		 RETURNING next_index - 1`,
		string(chain), tenantID,
	).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("treasury: allocate hd index for %s: %w", chain, err)
	}
	return uint32(next), nil
}

// selfCustodyProvider is the CollectionProvider adapter for HD-derived
// deposit addresses — registered under providerName "self_custody_wallet"
// (corridor.ProviderBinding.ProviderName), the same config-driven plugging
// mechanism the Busha adapter uses. Disabled until a seed has been loaded
// (see IsEnabled), so a misconfigured corridor binding fails closed rather
// than deriving from a nil seed.
type selfCustodyProvider struct {
	store *Store
}

func (p *selfCustodyProvider) Name() string    { return "self_custody_wallet" }
func (p *selfCustodyProvider) IsEnabled() bool { return p.store.seed != nil }

// CustodyType is required by the CollectionProvider interface's shared
// shape (see providers.go) even though self-custody's actual custody
// classification is chain-independent — always self_custody.
func (p *selfCustodyProvider) CustodyType() CustodyType { return CustodyTypeSelf }

// ReserveAddress expects cryptoNetwork to be one of the wallet.Chain
// constants ("bitcoin"/"ethereum"/"bsc"/"tron") — the convention this
// package establishes for corridor.crypto_network on self-custody-bound
// corridors, since no other convention was already fixed elsewhere.
// Derives from tenantID's own segregated HD account (see
// getOrAllocateTenantAccount) — never the platform-level account.
func (p *selfCustodyProvider) ReserveAddress(ctx context.Context, tenantID uuid.UUID, _, cryptoNetwork string) (ProviderAddress, error) {
	chain := wallet.Chain(cryptoNetwork)
	switch chain {
	case wallet.Bitcoin, wallet.Ethereum, wallet.BSC, wallet.Tron:
	default:
		return ProviderAddress{}, fmt.Errorf("treasury: unsupported self-custody chain %q", cryptoNetwork)
	}

	addr, err := p.store.allocateOrReuseAddress(ctx, tenantID, chain)
	if err != nil {
		return ProviderAddress{}, err
	}
	return ProviderAddress{Address: addr}, nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
