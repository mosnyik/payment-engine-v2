package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/settlement"
)

// settlementWebhookSignatureHeader is where a settlement provider's payout
// callback carries its HMAC signature (settlement.VerifyWebhookSignature).
// TODO: confirm this matches each real provider's actual header name once
// known — a placeholder, same status as providers.go's request/response
// shapes.
const settlementWebhookSignatureHeader = "X-Webhook-Signature"

// settlementHandlers implements Phase 6's inbound webhook and ops-facing
// admin surface. handleWebhook is deliberately unauthenticated by the
// gateway/adminauth middlewares — it's self-verified via
// settlement.VerifyWebhookSignature instead, same tier as /admin/login.
// Every other handler sits behind adminauth.Middleware.
type settlementHandlers struct {
	settlement *settlement.Store
}

// POST /webhooks/settlement/{providerName}
func (h *settlementHandlers) handleWebhook(w http.ResponseWriter, r *http.Request) {
	providerName := chi.URLParam(r, "providerName")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	signature := r.Header.Get(settlementWebhookSignatureHeader)

	err = h.settlement.HandleSettlementWebhook(r.Context(), providerName, body, signature)
	switch {
	case errors.Is(err, settlement.ErrInvalidWebhookSignature):
		writeErr(w, http.StatusUnauthorized, err)
		return
	case errors.Is(err, settlement.ErrUnknownProvider), errors.Is(err, settlement.ErrUnknownAttempt):
		writeErr(w, http.StatusNotFound, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// settlementResponse is the ops-facing wire shape — unlike sessionResponse,
// this is an internal admin surface, so nothing here needs to be withheld
// the way compliance_case_id is from the tenant-facing session response.
type settlementResponse struct {
	ID                  uuid.UUID `json:"id"`
	SessionID           uuid.UUID `json:"session_id"`
	TenantID            uuid.UUID `json:"tenant_id"`
	Status              string    `json:"status"`
	CryptoAsset         string    `json:"crypto_asset"`
	CryptoAmount        string    `json:"crypto_amount"`
	FiatCurrency        string    `json:"fiat_currency"`
	FiatValue           string    `json:"fiat_value"`
	FeeAmount           string    `json:"fee_amount"`
	TenantPayableAmount string    `json:"tenant_payable_amount"`
	AttemptCount        int       `json:"attempt_count"`
	OpsPaged            bool      `json:"ops_paged"`
	CreatedAt           string    `json:"created_at"`
}

func toSettlementResponse(st *settlement.Settlement) settlementResponse {
	return settlementResponse{
		ID:                  st.ID,
		SessionID:           st.SessionID,
		TenantID:            st.TenantID,
		Status:              string(st.Status),
		CryptoAsset:         st.CryptoAsset,
		CryptoAmount:        st.CryptoAmount.String(),
		FiatCurrency:        st.FiatCurrency,
		FiatValue:           st.FiatValue.String(),
		FeeAmount:           st.FeeAmount.String(),
		TenantPayableAmount: st.TenantPayableAmount.String(),
		AttemptCount:        st.AttemptCount,
		OpsPaged:            st.OpsPagedAt != nil,
		CreatedAt:           st.CreatedAt.Format(http.TimeFormat),
	}
}

// GET /admin/settlements?status=settlement_failed
func (h *settlementHandlers) listSettlements(w http.ResponseWriter, r *http.Request) {
	status := settlement.Status(r.URL.Query().Get("status"))
	if status == "" {
		status = settlement.StatusSettlementFailed
	}
	settlements, err := h.settlement.ListSettlements(r.Context(), status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]settlementResponse, len(settlements))
	for i := range settlements {
		resp[i] = toSettlementResponse(&settlements[i])
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /admin/settlements/{settlementID}/retry
// {"corrected_destination": {...opaque...}, "confirmed_failed": false}
func (h *settlementHandlers) retrySettlement(w http.ResponseWriter, r *http.Request) {
	settlementID, err := uuid.Parse(chi.URLParam(r, "settlementID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	adminID, ok := adminauth.AdminIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing admin identity"))
		return
	}

	var req struct {
		CorrectedDestination json.RawMessage `json:"corrected_destination"`
		ConfirmedFailed       bool            `json:"confirmed_failed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	st, err := h.settlement.RetryPayout(r.Context(), settlementID, adminID, req.CorrectedDestination, req.ConfirmedFailed)
	if errors.Is(err, settlement.ErrNotRetryable) {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettlementResponse(st))
}

// accountRefRequest is the wire shape for settlement.AccountRef — an
// explicit account picker, since ARCHITECTURE.md §8 only says a reversal
// resolution lands "wherever the funds actually ended up" without
// specifying a fixed taxonomy.
type accountRefRequest struct {
	TenantID    *uuid.UUID `json:"tenant_id,omitempty"`
	AccountType string     `json:"account_type"`
	AssetCode   string     `json:"asset_code"`
	UnitType    string     `json:"unit_type"`
	Name        string     `json:"name"`
}

func (a accountRefRequest) toAccountRef() settlement.AccountRef {
	return settlement.AccountRef{
		TenantID:    a.TenantID,
		AccountType: a.AccountType,
		AssetCode:   a.AssetCode,
		UnitType:    a.UnitType,
		Name:        a.Name,
	}
}

// POST /admin/settlements/reversals/{reversalID}/resolve
// {"debit": {...}, "credit": {...}, "amount": "100.00", "note": "..."}
func (h *settlementHandlers) resolveReversal(w http.ResponseWriter, r *http.Request) {
	reversalID, err := uuid.Parse(chi.URLParam(r, "reversalID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	adminID, ok := adminauth.AdminIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing admin identity"))
		return
	}

	var req struct {
		Debit  accountRefRequest `json:"debit"`
		Credit accountRefRequest `json:"credit"`
		Amount string            `json:"amount"`
		Note   string            `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("amount must be a valid decimal string"))
		return
	}

	err = h.settlement.ResolveReversal(r.Context(), reversalID, adminID, req.Debit.toAccountRef(), req.Credit.toAccountRef(), amount, req.Note)
	switch {
	case errors.Is(err, settlement.ErrReversalNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, settlement.ErrReversalAlreadyResolved):
		writeErr(w, http.StatusConflict, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
