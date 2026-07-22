package treasury

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

// ErrTenantWalletNotRegistered means a tenant hasn't registered a custom
// wallet address for the requested chain yet.
var ErrTenantWalletNotRegistered = errors.New("treasury: tenant has not registered a custom wallet for this chain")

// RegisterTenantCustomWallet records tenantID's own deposit address for
// chain — a tenant-supplied wallet the platform only monitors, never
// holds a key for, and never sweeps (see tenantProvidedWalletProvider
// below). Validates the address format for chain first
// (wallet.ValidateAddress) so a typo doesn't silently break monitoring
// forever. Upsert — a tenant can replace their registered address for a
// chain at any time; in-flight reservations against the old address are
// unaffected (they keep pointing at whatever address they were created
// against).
func (s *Store) RegisterTenantCustomWallet(ctx context.Context, tenantID uuid.UUID, chain wallet.Chain, address, addressTag string) error {
	if err := wallet.ValidateAddress(chain, address); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO treasury_tenant_custom_wallets (tenant_id, chain, address, address_tag)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, chain) DO UPDATE SET
		   address = EXCLUDED.address,
		   address_tag = EXCLUDED.address_tag,
		   registered_at = now()`,
		tenantID, string(chain), address, addressTag,
	)
	if err != nil {
		return fmt.Errorf("treasury: register tenant custom wallet: %w", err)
	}
	return nil
}

func (s *Store) getTenantCustomWallet(ctx context.Context, tenantID uuid.UUID, chain wallet.Chain) (address, addressTag string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT address, address_tag FROM treasury_tenant_custom_wallets WHERE tenant_id = $1 AND chain = $2`,
		tenantID, string(chain),
	).Scan(&address, &addressTag)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrTenantWalletNotRegistered
	}
	if err != nil {
		return "", "", fmt.Errorf("treasury: get tenant custom wallet: %w", err)
	}
	return address, addressTag, nil
}

// tenantProvidedWalletProvider is the CollectionProvider adapter for
// tenant-supplied deposit addresses — registered under providerName
// "tenant_provided_wallet" (corridor.ProviderBinding.ProviderName), the
// same config-driven plugging mechanism Busha/self-custody use. Unlike
// those two, this adapter never derives a key and never sweeps: the
// platform's role is limited to monitoring and notifying the tenant (see
// watcher.go's custody-type gate and tenant_notify.go).
type tenantProvidedWalletProvider struct {
	store *Store
}

func (p *tenantProvidedWalletProvider) Name() string    { return "tenant_provided_wallet" }
func (p *tenantProvidedWalletProvider) IsEnabled() bool { return true }

func (p *tenantProvidedWalletProvider) CustodyType() CustodyType { return CustodyTypeTenantProvided }

func (p *tenantProvidedWalletProvider) ReserveAddress(ctx context.Context, tenantID uuid.UUID, _, cryptoNetwork string) (ProviderAddress, error) {
	chain := wallet.Chain(cryptoNetwork)
	address, addressTag, err := p.store.getTenantCustomWallet(ctx, tenantID, chain)
	if err != nil {
		return ProviderAddress{}, err
	}
	return ProviderAddress{Address: address, AddressTag: addressTag}, nil
}
