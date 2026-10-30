package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/settlement"
	"github.com/sirfi/payment-engine-v2/internal/treasury"
)

// corridorAdminHandlers implements admin corridor management. Corridors are
// platform config: an admin creates one here, tenants then opt in via
// POST /v2/portal/corridors/{corridorID} (or an admin entitles them).
type corridorAdminHandlers struct {
	corridor   *corridor.Store
	settlement *settlement.Store
	treasury   *treasury.Store
	compliance *compliance.Store
	admin      *adminauth.Store
}

// defaultCorridorWindow matches the corridors table's column defaults for
// travel_rule_window_seconds and compliance_hold_timeout_seconds.
const defaultCorridorWindow = 24 * time.Hour

type providerBindingResponse struct {
	ID       uuid.UUID `json:"id"`
	Type     string    `json:"type"`
	Name     string    `json:"name"`
	Priority int       `json:"priority"`
	Active   bool      `json:"active"`
}

type createCorridorResponse struct {
	corridorResponse
	Providers []providerBindingResponse `json:"providers"`
}

type createCorridorRequest struct {
	CryptoAsset                  string   `json:"crypto_asset"`
	CryptoNetwork                string   `json:"crypto_network"`
	FiatCurrency                 string   `json:"fiat_currency"`
	Active                       *bool    `json:"active"`
	MinAmountFiat                *string  `json:"min_amount_fiat"`
	MaxAmountFiat                *string  `json:"max_amount_fiat"`
	TravelRuleThresholdFiat      *string  `json:"travel_rule_threshold_fiat"`
	TravelRuleWindowSeconds      *int     `json:"travel_rule_window_seconds"`
	ComplianceHoldTimeoutSeconds *int     `json:"compliance_hold_timeout_seconds"`
	RequiredDestinationFields    []string `json:"required_destination_fields"`
	Providers                    []struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Priority int    `json:"priority"`
	} `json:"providers"`
}

// parsePositiveAmount parses an optional decimal string field, rejecting
// zero/negative values. nil in, nil out.
func parsePositiveAmount(field string, v *string) (*decimal.Decimal, error) {
	if v == nil {
		return nil, nil
	}
	d, err := decimal.NewFromString(strings.TrimSpace(*v))
	if err != nil {
		return nil, fmt.Errorf("%s: not a decimal: %q", field, *v)
	}
	if !d.IsPositive() {
		return nil, fmt.Errorf("%s must be positive", field)
	}
	return &d, nil
}

// parseWindow turns an optional seconds field into a duration, defaulting
// to defaultCorridorWindow and rejecting zero/negative values.
func parseWindow(field string, v *int) (time.Duration, error) {
	if v == nil {
		return defaultCorridorWindow, nil
	}
	if *v <= 0 {
		return 0, fmt.Errorf("%s must be positive", field)
	}
	return time.Duration(*v) * time.Second, nil
}

// validate checks req and converts it into corridor.CreateCorridor's
// inputs. Provider names are checked against the adapters actually
// registered in this process, so a typo can't create a corridor whose
// sessions always fail to find a provider.
func (h *corridorAdminHandlers) validate(req createCorridorRequest) (corridor.UpsertCorridorInput, []corridor.NewProviderBinding, error) {
	in := corridor.UpsertCorridorInput{
		CryptoAsset:   strings.ToUpper(strings.TrimSpace(req.CryptoAsset)),
		CryptoNetwork: strings.ToUpper(strings.TrimSpace(req.CryptoNetwork)),
		FiatCurrency:  strings.ToUpper(strings.TrimSpace(req.FiatCurrency)),
		Active:        req.Active == nil || *req.Active,
	}
	if in.CryptoAsset == "" || in.CryptoNetwork == "" || in.FiatCurrency == "" {
		return in, nil, errors.New("crypto_asset, crypto_network and fiat_currency are required")
	}

	var err error
	if in.MinAmountFiat, err = parsePositiveAmount("min_amount_fiat", req.MinAmountFiat); err != nil {
		return in, nil, err
	}
	if in.MaxAmountFiat, err = parsePositiveAmount("max_amount_fiat", req.MaxAmountFiat); err != nil {
		return in, nil, err
	}
	if in.MinAmountFiat != nil && in.MaxAmountFiat != nil && in.MinAmountFiat.GreaterThan(*in.MaxAmountFiat) {
		return in, nil, errors.New("min_amount_fiat must not exceed max_amount_fiat")
	}
	if in.TravelRuleThresholdFiat, err = parsePositiveAmount("travel_rule_threshold_fiat", req.TravelRuleThresholdFiat); err != nil {
		return in, nil, err
	}
	if in.TravelRuleWindow, err = parseWindow("travel_rule_window_seconds", req.TravelRuleWindowSeconds); err != nil {
		return in, nil, err
	}
	if in.ComplianceHoldTimeout, err = parseWindow("compliance_hold_timeout_seconds", req.ComplianceHoldTimeoutSeconds); err != nil {
		return in, nil, err
	}

	for _, f := range req.RequiredDestinationFields {
		if f = strings.TrimSpace(f); f == "" {
			return in, nil, errors.New("required_destination_fields must not contain empty names")
		}
		in.RequiredDestinationFields = append(in.RequiredDestinationFields, f)
	}

	bindings := make([]corridor.NewProviderBinding, 0, len(req.Providers))
	seen := make(map[string]bool)
	for _, p := range req.Providers {
		name := strings.TrimSpace(p.Name)
		var known bool
		switch corridor.ProviderType(p.Type) {
		case corridor.ProviderTypeCollection:
			known = h.treasury.HasProvider(name)
		case corridor.ProviderTypeSettlement:
			known = h.settlement.HasProvider(name)
		case corridor.ProviderTypeCompliance:
			known = h.compliance.HasProvider(name)
		default:
			return in, nil, fmt.Errorf("provider type %q must be collection, settlement or compliance", p.Type)
		}
		if !known {
			return in, nil, fmt.Errorf("unknown %s provider %q", p.Type, name)
		}
		if p.Priority < 0 {
			return in, nil, fmt.Errorf("%s provider %q: priority must not be negative", p.Type, name)
		}
		key := p.Type + "/" + name
		if seen[key] {
			return in, nil, fmt.Errorf("duplicate %s provider %q", p.Type, name)
		}
		seen[key] = true
		bindings = append(bindings, corridor.NewProviderBinding{
			ProviderType: corridor.ProviderType(p.Type),
			ProviderName: name,
			Priority:     p.Priority,
		})
	}
	return in, bindings, nil
}

// POST /admin/corridors — creates a corridor plus its provider bindings in
// one transaction. Create-only: an existing asset/network/fiat combination
// is a 409, never an overwrite.
func (h *corridorAdminHandlers) createCorridor(w http.ResponseWriter, r *http.Request) {
	var req createCorridorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	in, bindings, err := h.validate(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	c, created, err := h.corridor.CreateCorridor(r.Context(), in, bindings)
	if errors.Is(err, corridor.ErrAlreadyExists) {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	resp := createCorridorResponse{corridorResponse: toCorridorResponse(c), Providers: make([]providerBindingResponse, len(created))}
	for i, b := range created {
		resp.Providers[i] = providerBindingResponse{ID: b.ID, Type: string(b.ProviderType), Name: b.ProviderName, Priority: b.Priority, Active: b.Active}
	}

	// The corridor is already committed, so an audit-log failure is logged
	// rather than failing the request (the request audit log still has it).
	if adminID, ok := adminauth.AdminIDFromContext(r.Context()); ok {
		if err := h.admin.LogAction(r.Context(), adminID, "corridor.create", c.ID.String(), resp); err != nil {
			log.Printf("admin: corridor %s created but audit log write failed: %v", c.ID, err)
		}
	}

	writeJSON(w, http.StatusCreated, resp)
}
