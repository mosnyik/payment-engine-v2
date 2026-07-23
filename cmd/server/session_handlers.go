package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
	"github.com/sirfi/payment-engine-v2/internal/session"
)

// sessionHandlers implements Phase 5's tenant-facing surface. Both routes
// sit behind the tenant gateway's HMAC auth (never adminauth) — a session
// belongs to whichever tenant the request authenticated as.
type sessionHandlers struct {
	session *session.Store
}

// sessionResponse is the wire shape returned to a tenant — deliberately
// narrower than session.Session: compliance_case_id is never included,
// consistent with ARCHITECTURE.md §5's rule that a tenant only ever sees a
// generic status, never the specific screening reason behind a hold.
type sessionResponse struct {
	ID                   uuid.UUID  `json:"id"`
	Status               string     `json:"status"`
	FiatCurrency         string     `json:"fiat_currency"`
	FiatAmount           string     `json:"fiat_amount"`
	CryptoAsset          string     `json:"crypto_asset"`
	CryptoNetwork        string     `json:"crypto_network"`
	DepositReservationID *uuid.UUID `json:"deposit_reservation_id,omitempty"`
	CreatedAt            string     `json:"created_at"`
}

func toSessionResponse(s *session.Session) sessionResponse {
	return sessionResponse{
		ID:                   s.ID,
		Status:               string(s.Status),
		FiatCurrency:         s.FiatCurrency,
		FiatAmount:           s.FiatAmount.String(),
		CryptoAsset:          s.CryptoAsset,
		CryptoNetwork:        s.CryptoNetwork,
		DepositReservationID: s.DepositReservationID,
		CreatedAt:            s.CreatedAt.Format(http.TimeFormat),
	}
}

// POST /v2/sessions {"crypto_asset": "...", "crypto_network": "...", "fiat_currency": "...", "fiat_amount": "100.00"}
// tenant_id always comes from the authenticated gateway context, never the
// body — a tenant can only ever create a session for itself.
func (h *sessionHandlers) createSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := gateway.TenantIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}

	var req struct {
		CryptoAsset   string `json:"crypto_asset"`
		CryptoNetwork string `json:"crypto_network"`
		FiatCurrency  string `json:"fiat_currency"`
		FiatAmount    string `json:"fiat_amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	fiatAmount, err := decimal.NewFromString(req.FiatAmount)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("fiat_amount must be a valid decimal string"))
		return
	}

	sess, err := h.session.CreateSession(r.Context(), tenantID, req.CryptoAsset, req.CryptoNetwork, req.FiatCurrency, fiatAmount)
	switch {
	case errors.Is(err, session.ErrCorridorNotSupported), errors.Is(err, session.ErrNotEntitled):
		writeErr(w, http.StatusBadRequest, err)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSessionResponse(sess))
}

// GET /v2/sessions/{sessionID}. Not found is returned both when the
// session truly doesn't exist and when it belongs to a different tenant —
// never a 403, so a tenant can't use this endpoint to confirm another
// tenant's session ID exists.
func (h *sessionHandlers) getSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := gateway.TenantIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	sess, err := h.session.GetSession(r.Context(), id)
	if errors.Is(err, session.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if sess.TenantID != tenantID {
		writeErr(w, http.StatusNotFound, session.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, toSessionResponse(sess))
}
