package treasury

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// CustodyType records which custody model a collection provider uses — a
// property of the provider, not fixed system-wide (see ARCHITECTURE.md §2).
type CustodyType string

const (
	CustodyTypeSelf    CustodyType = "self_custody"
	CustodyTypePartner CustodyType = "partner_custodied"
	// CustodyTypeTenantProvided is a tenant-supplied deposit address: the
	// platform monitors it and notifies the tenant on detection/
	// confirmation, but never holds a key for it and never sweeps it
	// (see tenant_wallet.go).
	CustodyTypeTenantProvided CustodyType = "tenant_provided"
)

// ProviderAddress is what a CollectionProvider hands back for one deposit
// address reservation.
type ProviderAddress struct {
	Address           string
	AddressTag        string // memo/tag, empty when the chain doesn't use one
	ProviderReference string // the provider's own id for this address/wallet
}

// CollectionProvider is a crypto-collection adapter bound to a corridor via
// corridor.ProviderBinding (provider_type = collection). Reserving an
// address is assumed cheap enough to call inline — unlike rate.Provider,
// there's no cached-read/live-fetch split here because there's nothing to
// poll ahead of time: an address is requested only when a real caller
// (Phase 5's session module) needs one.
type CollectionProvider interface {
	Name() string
	IsEnabled() bool
	CustodyType() CustodyType
	ReserveAddress(ctx context.Context, cryptoAsset, cryptoNetwork string) (ProviderAddress, error)
}

// CollectionProviderConfig configures one external collection-provider
// adapter. Mirrors rate.ProviderConfig, plus WebhookSecret for verifying
// inbound deposit callbacks.
type CollectionProviderConfig struct {
	Enabled       bool
	APIURL        string
	APIKey        string
	WebhookSecret string
}

const httpProviderTimeout = 8 * time.Second

// bushaProvider is the Busha (partner-custodied) collection adapter.
//
// TODO: plug in the real Busha collection API.
//  1. Set BUSHA_TREASURY_ENABLED=true, BUSHA_TREASURY_API_URL,
//     BUSHA_TREASURY_API_KEY, BUSHA_TREASURY_WEBHOOK_SECRET.
//  2. Update reserveAddressRequest with the correct path/auth headers.
//  3. Update parseReserveAddressResponse to extract address/tag/reference
//     from the real response shape.
//  4. Update VerifyWebhookSignature (webhook.go) with Busha's real
//     signature scheme once known.
//
// No real endpoint/response shape is supplied yet — same TODO-stub
// convention already established for rate/providers.go's Busha/LiquidRamp/
// Anchor adapters. Disabled by default.
type bushaProvider struct {
	cfg    CollectionProviderConfig
	client *http.Client
}

func newBushaProvider(cfg CollectionProviderConfig) *bushaProvider {
	return &bushaProvider{
		cfg:    cfg,
		client: &http.Client{Timeout: httpProviderTimeout},
	}
}

func (p *bushaProvider) Name() string             { return "busha" }
func (p *bushaProvider) IsEnabled() bool          { return p.cfg.Enabled }
func (p *bushaProvider) CustodyType() CustodyType { return CustodyTypePartner }

func (p *bushaProvider) ReserveAddress(ctx context.Context, cryptoAsset, cryptoNetwork string) (ProviderAddress, error) {
	req, err := p.buildRequest(ctx, cryptoAsset, cryptoNetwork)
	if err != nil {
		return ProviderAddress{}, fmt.Errorf("treasury: build busha request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return ProviderAddress{}, fmt.Errorf("treasury: call busha: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ProviderAddress{}, fmt.Errorf("treasury: busha returned status %d", resp.StatusCode)
	}

	// TODO: replace with the real Busha response shape.
	var parsed struct {
		Address     string `json:"address"`
		Tag         string `json:"tag"`
		ReferenceID string `json:"reference_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ProviderAddress{}, fmt.Errorf("treasury: parse busha response: %w", err)
	}
	if parsed.Address == "" {
		return ProviderAddress{}, fmt.Errorf("treasury: busha response missing address")
	}

	return ProviderAddress{
		Address:           parsed.Address,
		AddressTag:        parsed.Tag,
		ProviderReference: parsed.ReferenceID,
	}, nil
}

func (p *bushaProvider) buildRequest(ctx context.Context, cryptoAsset, cryptoNetwork string) (*http.Request, error) {
	// TODO: replace with the real Busha deposit-address endpoint/payload
	// shape — asset/network field names below are a placeholder guess.
	body, err := json.Marshal(struct {
		Asset   string `json:"asset"`
		Network string `json:"network"`
	}{Asset: cryptoAsset, Network: cryptoNetwork})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, p.cfg.APIURL+"/TODO/addresses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req, nil
}
