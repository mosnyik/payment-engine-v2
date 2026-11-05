package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/kyc"
	"github.com/sirfi/payment-engine-v2/internal/platform/audit"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
)

type kycHandlers struct {
	kyc *kyc.Service
}

type kycVerifyResponse struct {
	CheckID   uuid.UUID                 `json:"check_id"`
	IDType    kyc.IDType                `json:"id_type"`
	Result    kyc.Result                `json:"result"`
	Checks    map[string]kyc.FieldCheck `json:"checks"`
	CreatedAt time.Time                 `json:"created_at"`
}

// POST /v2/kyc/verify — KYC as a service for builders. Tenant HMAC auth,
// and the key must be explicitly granted kyc:verify (every call is billed
// by the provider). Returns only whether the details match the official
// record, never the record itself.
func (h *kycHandlers) verify(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := gateway.TenantIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	var apiKeyID *uuid.UUID
	if id, ok := audit.IdentityFromContext(r.Context()); ok {
		apiKeyID = id.APIKeyID
	}

	var req struct {
		IDType      kyc.IDType `json:"id_type"`
		IDNumber    string     `json:"id_number"`
		FirstName   string     `json:"first_name"`
		LastName    string     `json:"last_name"`
		DateOfBirth string     `json:"date_of_birth"`
		Consent     bool       `json:"consent"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body"))
		return
	}

	check, err := h.kyc.Verify(r.Context(), tenantID, apiKeyID, kyc.Request{
		IDType:      req.IDType,
		IDNumber:    req.IDNumber,
		FirstName:   req.FirstName,
		LastName:    req.LastName,
		DateOfBirth: req.DateOfBirth,
		Consent:     req.Consent,
	})
	switch {
	case errors.Is(err, kyc.ErrInvalidRequest):
		writeErr(w, http.StatusBadRequest, err)
		return
	case errors.Is(err, kyc.ErrNotConfigured):
		writeErr(w, http.StatusServiceUnavailable, kyc.ErrNotConfigured)
		return
	case errors.Is(err, kyc.ErrUnavailable):
		// The detail (provider status, e.g. out of credit) is for ops only.
		log.Printf("kyc: verify for tenant %s: %v", tenantID, err)
		writeErr(w, http.StatusServiceUnavailable, kyc.ErrUnavailable)
		return
	case err != nil:
		log.Printf("kyc: verify for tenant %s: %v", tenantID, err)
		writeErr(w, http.StatusInternalServerError, errors.New("internal error"))
		return
	}

	writeJSON(w, http.StatusOK, kycVerifyResponse{
		CheckID:   check.ID,
		IDType:    check.IDType,
		Result:    check.Result,
		Checks:    check.Checks,
		CreatedAt: check.CreatedAt,
	})
}
