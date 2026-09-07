package treasury

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
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
	Amount            string
	ExpiresAt         *time.Time
}

// DepositRequest supplies the amount required by quote-based collection APIs.
type DepositRequest struct {
	Amount decimal.Decimal
}

type quotedCollectionProvider interface {
	ReserveDeposit(context.Context, uuid.UUID, string, string, DepositRequest) (ProviderAddress, error)
}

// CollectionProvider is a crypto-collection adapter bound to a corridor via
// corridor.ProviderBinding (provider_type = collection). Reserving an
// address is assumed cheap enough to call inline — unlike rate.Provider,
// there's no cached-read/live-fetch split here because there's nothing to
// poll ahead of time: an address is requested only when a real caller
// (Phase 5's session module) needs one. tenantID identifies whose deposit
// this is — self-custody derives from that tenant's own segregated HD
// account (see hdwallet.go), and the tenant-provided-wallet adapter looks
// up that tenant's registered address; bushaProvider currently ignores it
// because collection currently credits the configured business balance.
type CollectionProvider interface {
	Name() string
	IsEnabled() bool
	CustodyType() CustodyType
	ReserveAddress(ctx context.Context, tenantID uuid.UUID, cryptoAsset, cryptoNetwork string) (ProviderAddress, error)
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

// bushaProvider collects into a Busha Business balance using quotes and transfers.
// See docs/BUSHA_SANDBOX.md for sandbox configuration and API sources.
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

func (p *bushaProvider) ReserveAddress(context.Context, uuid.UUID, string, string) (ProviderAddress, error) {
	return ProviderAddress{}, fmt.Errorf("treasury: Busha Business requires a deposit amount")
}

// ReserveDeposit creates a same-currency quote and finalizes it to generate
// a deposit address in the configured business account. It never requests a payout.
func (p *bushaProvider) ReserveDeposit(ctx context.Context, _ uuid.UUID, asset, network string, deposit DepositRequest) (ProviderAddress, error) {
	if !p.cfg.Enabled || strings.TrimSpace(p.cfg.APIKey) == "" || strings.TrimSpace(p.cfg.WebhookSecret) == "" {
		return ProviderAddress{}, fmt.Errorf("treasury: Busha collection is disabled or missing credentials")
	}
	if !deposit.Amount.IsPositive() || asset == "" || network == "" {
		return ProviderAddress{}, fmt.Errorf("treasury: Busha requires a positive amount, asset and network")
	}
	var quote struct {
		ID string `json:"id"`
	}
	err := p.post(ctx, "/v1/quotes", map[string]any{
		"source_currency": asset, "target_currency": asset,
		"source_amount": deposit.Amount.String(),
		"pay_in":        map[string]string{"type": "address", "network": network},
	}, &quote)
	if err != nil {
		return ProviderAddress{}, err
	}
	if quote.ID == "" {
		return ProviderAddress{}, fmt.Errorf("treasury: Busha quote missing id")
	}
	var transfer bushaTransfer
	if err := p.post(ctx, "/v1/transfers", map[string]string{"quote_id": quote.ID}, &transfer); err != nil {
		return ProviderAddress{}, err
	}
	if transfer.ID == "" || transfer.PayIn.Address == "" || transfer.PayIn.Type != "address" ||
		transfer.SourceCurrency != asset || transfer.TargetCurrency != asset || transfer.PayIn.Network != network ||
		!transfer.SourceAmount.Equal(deposit.Amount) || transfer.PayIn.ExpiresAt == nil || !transfer.PayIn.ExpiresAt.After(time.Now()) {
		return ProviderAddress{}, fmt.Errorf("treasury: Busha returned incomplete or mismatched deposit instructions")
	}
	return ProviderAddress{Address: transfer.PayIn.Address, AddressTag: transfer.PayIn.Memo,
		ProviderReference: transfer.ID, Amount: transfer.SourceAmount.String(), ExpiresAt: transfer.PayIn.ExpiresAt}, nil
}

type bushaTransfer struct {
	ID             string          `json:"id"`
	SourceCurrency string          `json:"source_currency"`
	TargetCurrency string          `json:"target_currency"`
	SourceAmount   decimal.Decimal `json:"source_amount"`
	Status         string          `json:"status"`
	PayIn          struct {
		Type      string     `json:"type"`
		Address   string     `json:"address"`
		Network   string     `json:"network"`
		Memo      string     `json:"memo"`
		ExpiresAt *time.Time `json:"expires_at"`
	} `json:"pay_in"`
}

func (p *bushaProvider) post(ctx context.Context, path string, payload, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.cfg.APIURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("treasury: build Busha request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("treasury: Busha %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("treasury: Busha %s returned HTTP %d", path, resp.StatusCode)
	}
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("treasury: decode Busha response: %w", err)
	}
	if envelope.Status != "success" || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("treasury: Busha response was not successful")
	}
	return json.Unmarshal(envelope.Data, result)
}
