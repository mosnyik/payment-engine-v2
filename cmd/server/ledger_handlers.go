package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
)

// ledgerHandlers implements Phase 8's reconciliation ops surface — same
// shape as settlementHandlers'/notificationHandlers' ops routes, all
// admin-auth-gated.
type ledgerHandlers struct {
	ledger *ledger.Ledger
}

type discrepancyResponse struct {
	ID              uuid.UUID  `json:"id"`
	AccountID       uuid.UUID  `json:"account_id"`
	CachedBalance   string     `json:"cached_balance"`
	ComputedBalance string     `json:"computed_balance"`
	DriftAmount     string     `json:"drift_amount"`
	DetectedAt      string     `json:"detected_at"`
	ResolvedAt      *string    `json:"resolved_at,omitempty"`
	ResolvedBy      *uuid.UUID `json:"resolved_by,omitempty"`
	ResolutionNote  *string    `json:"resolution_note,omitempty"`
}

func toDiscrepancyResponse(d *ledger.Discrepancy) discrepancyResponse {
	resp := discrepancyResponse{
		ID:              d.ID,
		AccountID:       d.AccountID,
		CachedBalance:   d.CachedBalance.String(),
		ComputedBalance: d.ComputedBalance.String(),
		DriftAmount:     d.DriftAmount.String(),
		DetectedAt:      d.DetectedAt.Format(http.TimeFormat),
		ResolvedBy:      d.ResolvedBy,
		ResolutionNote:  d.ResolutionNote,
	}
	if d.ResolvedAt != nil {
		formatted := d.ResolvedAt.Format(http.TimeFormat)
		resp.ResolvedAt = &formatted
	}
	return resp
}

// GET /admin/ledger/discrepancies?status=open — defaults to open, the ops
// queue view; ?status=resolved lists closed ones.
func (h *ledgerHandlers) listDiscrepancies(w http.ResponseWriter, r *http.Request) {
	resolved := r.URL.Query().Get("status") == "resolved"
	discrepancies, err := h.ledger.ListDiscrepancies(r.Context(), resolved)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]discrepancyResponse, len(discrepancies))
	for i := range discrepancies {
		resp[i] = toDiscrepancyResponse(&discrepancies[i])
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /admin/ledger/discrepancies/{discrepancyID}/resolve {"note": "..."}
// Never rewrites the cached balance — see internal/ledger/reconcile.go's
// package doc comment.
func (h *ledgerHandlers) resolveDiscrepancy(w http.ResponseWriter, r *http.Request) {
	discrepancyID, err := uuid.Parse(chi.URLParam(r, "discrepancyID"))
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
		Note string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	d, err := h.ledger.ResolveDiscrepancy(r.Context(), discrepancyID, adminID, req.Note)
	if errors.Is(err, ledger.ErrDiscrepancyAlreadyResolved) {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, toDiscrepancyResponse(d))
}
