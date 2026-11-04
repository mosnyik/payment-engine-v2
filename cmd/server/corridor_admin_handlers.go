package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
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
	CryptoAsset                  string                 `json:"crypto_asset"`
	CryptoNetwork                string                 `json:"crypto_network"`
	FiatCurrency                 string                 `json:"fiat_currency"`
	Active                       *bool                  `json:"active"`
	MinAmountFiat                *string                `json:"min_amount_fiat"`
	MaxAmountFiat                *string                `json:"max_amount_fiat"`
	TravelRuleThresholdFiat      *string                `json:"travel_rule_threshold_fiat"`
	TravelRuleWindowSeconds      *int                   `json:"travel_rule_window_seconds"`
	ComplianceHoldTimeoutSeconds *int                   `json:"compliance_hold_timeout_seconds"`
	RequiredDestinationFields    []string               `json:"required_destination_fields"`
	Providers                    []providerBindingInput `json:"providers"`
}

type providerBindingInput struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Priority int    `json:"priority"`
}

func toProviderBindingResponses(bindings []corridor.ProviderBinding) []providerBindingResponse {
	out := make([]providerBindingResponse, len(bindings))
	for i, b := range bindings {
		out[i] = providerBindingResponse{ID: b.ID, Type: string(b.ProviderType), Name: b.ProviderName, Priority: b.Priority, Active: b.Active}
	}
	return out
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

	if in.RequiredDestinationFields, err = parseDestinationFields(req.RequiredDestinationFields); err != nil {
		return in, nil, err
	}

	bindings, err := h.validateProviders(req.Providers)
	if err != nil {
		return in, nil, err
	}
	return in, bindings, nil
}

// parseDestinationFields trims each required destination field name,
// rejecting empty ones.
func parseDestinationFields(raw []string) ([]string, error) {
	var out []string
	for _, f := range raw {
		if f = strings.TrimSpace(f); f == "" {
			return nil, errors.New("required_destination_fields must not contain empty names")
		}
		out = append(out, f)
	}
	return out, nil
}

// validateProviders checks each binding's type, that its name is an adapter
// actually registered in this process (so a typo can't produce a corridor
// whose sessions never find a provider), and rejects duplicates and
// negative priorities.
func (h *corridorAdminHandlers) validateProviders(providers []providerBindingInput) ([]corridor.NewProviderBinding, error) {
	bindings := make([]corridor.NewProviderBinding, 0, len(providers))
	seen := make(map[string]bool)
	for _, p := range providers {
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
			return nil, fmt.Errorf("provider type %q must be collection, settlement or compliance", p.Type)
		}
		if !known {
			return nil, fmt.Errorf("unknown %s provider %q", p.Type, name)
		}
		if p.Priority < 0 {
			return nil, fmt.Errorf("%s provider %q: priority must not be negative", p.Type, name)
		}
		key := p.Type + "/" + name
		if seen[key] {
			return nil, fmt.Errorf("duplicate %s provider %q", p.Type, name)
		}
		seen[key] = true
		bindings = append(bindings, corridor.NewProviderBinding{
			ProviderType: corridor.ProviderType(p.Type),
			ProviderName: name,
			Priority:     p.Priority,
		})
	}
	return bindings, nil
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

	resp := createCorridorResponse{corridorResponse: toCorridorResponse(c), Providers: toProviderBindingResponses(created)}

	// The corridor is already committed, so an audit-log failure is logged
	// rather than failing the request (the request audit log still has it).
	if adminID, ok := adminauth.AdminIDFromContext(r.Context()); ok {
		if err := h.admin.LogAction(r.Context(), adminID, "corridor.create", c.ID.String(), resp); err != nil {
			log.Printf("admin: corridor %s created but audit log write failed: %v", c.ID, err)
		}
	}

	writeJSON(w, http.StatusCreated, resp)
}

// immutableCorridorFields are the natural key. Sessions, tenant
// entitlements and the KYB jurisdiction gate are all tied to them, so a
// change means creating a new corridor and deactivating this one.
var immutableCorridorFields = map[string]bool{"crypto_asset": true, "crypto_network": true, "fiat_currency": true}

// applyCorridorPatch merges a PATCH body into c. Absent keys leave a field
// unchanged; an explicit null clears min/max/travel-rule threshold. Unknown
// keys are rejected so a typo isn't silently ignored. Validation runs on
// the merged result, so e.g. a new max is still checked against the
// existing min.
func applyCorridorPatch(c *corridor.Corridor, patch map[string]json.RawMessage) error {
	isNull := func(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }
	amount := func(field string, raw json.RawMessage) (*decimal.Decimal, error) {
		if isNull(raw) {
			return nil, nil
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s must be a decimal string or null", field)
		}
		return parsePositiveAmount(field, &v)
	}
	window := func(field string, raw json.RawMessage) (time.Duration, error) {
		var v int
		if isNull(raw) || json.Unmarshal(raw, &v) != nil {
			return 0, fmt.Errorf("%s must be a positive integer", field)
		}
		return parseWindow(field, &v)
	}

	if len(patch) == 0 {
		return errors.New("no fields to update")
	}
	var err error
	for key, raw := range patch {
		switch {
		case immutableCorridorFields[key]:
			return fmt.Errorf("%s can't be changed; create a new corridor and deactivate this one", key)
		case key == "active":
			if isNull(raw) || json.Unmarshal(raw, &c.Active) != nil {
				return errors.New("active must be a boolean")
			}
		case key == "min_amount_fiat":
			c.MinAmountFiat, err = amount(key, raw)
		case key == "max_amount_fiat":
			c.MaxAmountFiat, err = amount(key, raw)
		case key == "travel_rule_threshold_fiat":
			c.TravelRuleThresholdFiat, err = amount(key, raw)
		case key == "travel_rule_window_seconds":
			c.TravelRuleWindow, err = window(key, raw)
		case key == "compliance_hold_timeout_seconds":
			c.ComplianceHoldTimeout, err = window(key, raw)
		case key == "required_destination_fields":
			var fields []string
			if !isNull(raw) && json.Unmarshal(raw, &fields) != nil {
				return errors.New("required_destination_fields must be an array of strings")
			}
			c.RequiredDestinationFields, err = parseDestinationFields(fields)
		default:
			return fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			return err
		}
	}
	if c.MinAmountFiat != nil && c.MaxAmountFiat != nil && c.MinAmountFiat.GreaterThan(*c.MaxAmountFiat) {
		return errors.New("min_amount_fiat must not exceed max_amount_fiat")
	}
	return nil
}

// errCorridorPatchInvalid tags applyCorridorPatch failures coming back out
// of UpdateCorridor, so the handler can tell a 400 from a database error.
var errCorridorPatchInvalid = errors.New("invalid corridor update")

// PATCH /admin/corridors/{corridorID} — partial update of a corridor's
// settings, including deactivation ({"active": false}). A deactivated
// corridor rejects new sessions and disappears from the tenant portal;
// in-flight sessions finish normally and tenant entitlements are kept, so
// reactivating restores access.
func (h *corridorAdminHandlers) updateCorridor(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "corridorID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	var patchErr error
	before, after, err := h.corridor.UpdateCorridor(r.Context(), id, func(c *corridor.Corridor) error {
		if patchErr = applyCorridorPatch(c, patch); patchErr != nil {
			return errCorridorPatchInvalid
		}
		return nil
	})
	switch {
	case errors.Is(err, corridor.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, errCorridorPatchInvalid):
		writeErr(w, http.StatusBadRequest, patchErr)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	resp := toCorridorResponse(after)
	if adminID, ok := adminauth.AdminIDFromContext(r.Context()); ok {
		metadata := map[string]any{"before": toCorridorResponse(before), "after": resp}
		if err := h.admin.LogAction(r.Context(), adminID, "corridor.update", id.String(), metadata); err != nil {
			log.Printf("admin: corridor %s updated but audit log write failed: %v", id, err)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// PUT /admin/corridors/{corridorID}/providers {"providers": [...]} —
// replaces the corridor's whole set of active provider bindings atomically
// (same validation as create). Bindings left out are deactivated, not
// deleted. An empty list is allowed and deactivates every binding.
func (h *corridorAdminHandlers) replaceProviders(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "corridorID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Providers *[]providerBindingInput `json:"providers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Providers == nil {
		writeErr(w, http.StatusBadRequest, errors.New("providers is required (use [] to remove every binding)"))
		return
	}
	bindings, err := h.validateProviders(*req.Providers)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	replaced, err := h.corridor.ReplaceProviderBindings(r.Context(), id, bindings)
	if errors.Is(err, corridor.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	resp := map[string][]providerBindingResponse{"providers": toProviderBindingResponses(replaced)}
	if adminID, ok := adminauth.AdminIDFromContext(r.Context()); ok {
		if err := h.admin.LogAction(r.Context(), adminID, "corridor.providers.replace", id.String(), resp); err != nil {
			log.Printf("admin: corridor %s providers replaced but audit log write failed: %v", id, err)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
