package treasury

import (
	"context"
	"errors"
	"fmt"

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

// allocateOrReuseAddress returns a self-custody deposit address for chain,
// reusing a currently-unreserved previously-derived address when one
// exists (ARCHITECTURE.md §3: "Reuse per end-user") and deriving a new HD
// index only when none is free. Safe under concurrent callers: the actual
// safety net is the partial unique index on
// treasury_address_reservations(address) WHERE status = 'reserved' (see
// migration 000011) — a race here degrades to a persistReservation error,
// not a silently double-booked address, which is the actual audit-lesson-3
// fix; this function's reuse-first check is an optimization on top of it,
// not the safety mechanism itself.
func (s *Store) allocateOrReuseAddress(ctx context.Context, chain wallet.Chain) (string, error) {
	if s.seed == nil {
		return "", ErrHDWalletNotInitialized
	}

	if addr, ok, err := s.findFreeDerivedAddress(ctx, chain); err != nil {
		return "", err
	} else if ok {
		return addr, nil
	}

	index, err := s.allocateNextIndex(ctx, chain)
	if err != nil {
		return "", err
	}
	addr, err := wallet.DeriveAddress(s.seed, chain, index)
	if err != nil {
		return "", fmt.Errorf("treasury: derive address for %s index %d: %w", chain, index, err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO derived_addresses (chain, derivation_index, address) VALUES ($1, $2, $3)`,
		string(chain), index, addr,
	)
	if err != nil {
		return "", fmt.Errorf("treasury: persist derived address: %w", err)
	}
	return addr, nil
}

func (s *Store) findFreeDerivedAddress(ctx context.Context, chain wallet.Chain) (string, bool, error) {
	var addr string
	err := s.pool.QueryRow(ctx,
		`SELECT da.address
		 FROM derived_addresses da
		 LEFT JOIN treasury_address_reservations r
		   ON r.address = da.address AND r.status = 'reserved'
		 WHERE da.chain = $1 AND r.id IS NULL
		 ORDER BY da.derivation_index ASC
		 LIMIT 1`,
		string(chain),
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
// derivation index for chain — a single INSERT ... ON CONFLICT statement,
// so no explicit row lock/transaction is needed at this call site.
func (s *Store) allocateNextIndex(ctx context.Context, chain wallet.Chain) (uint32, error) {
	var next uint64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO hd_wallet_indices (chain, next_index) VALUES ($1, 1)
		 ON CONFLICT (chain) DO UPDATE SET
		   next_index = hd_wallet_indices.next_index + 1,
		   updated_at = now()
		 RETURNING next_index - 1`,
		string(chain),
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
func (p *selfCustodyProvider) ReserveAddress(ctx context.Context, _, cryptoNetwork string) (ProviderAddress, error) {
	chain := wallet.Chain(cryptoNetwork)
	switch chain {
	case wallet.Bitcoin, wallet.Ethereum, wallet.BSC, wallet.Tron:
	default:
		return ProviderAddress{}, fmt.Errorf("treasury: unsupported self-custody chain %q", cryptoNetwork)
	}

	addr, err := p.store.allocateOrReuseAddress(ctx, chain)
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
