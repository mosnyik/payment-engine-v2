package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

// adminHandlers implements the onboarding workflow's admin-facing actions:
// registration -> KYB submission -> review -> credential issuance ->
// corridor entitlement -> webhook config (ARCHITECTURE.md §5). Every
// handler except login sits behind adminauth.Middleware, never the tenant
// gateway's auth.
type adminHandlers struct {
	tenant     *tenant.Store
	compliance *compliance.Store
	admin      *adminauth.Store
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// POST /admin/login {"email": "...", "password": "..."} — the only
// unauthenticated route under /admin. There is no self-service admin
// signup (adminauth.CreateAdmin is a provisioning-script action, not an
// HTTP endpoint) — this only exchanges existing credentials for a session token.
func (h *adminHandlers) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	token, err := h.admin.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// POST /admin/tenants {"name": "..."} — step 1, registration.
func (h *adminHandlers) createTenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}

	id, err := h.tenant.CreateTenant(r.Context(), req.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id.String()})
}

// POST /admin/tenants/{tenantID}/kyb {"submitted_data": {...}, "provider_name": ""} — step 2+3.
// provider_name empty (the default — no vendor selected yet) always lands
// in the manual hold queue for a human to resolve via resolveHold below.
func (h *adminHandlers) submitKYB(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	var req struct {
		SubmittedData json.RawMessage `json:"submitted_data"`
		ProviderName  string          `json:"provider_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	c, err := h.compliance.ScreenTenant(r.Context(), tenantID, req.SubmittedData, req.ProviderName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// GET /admin/compliance/holds?case_type=kyb — the ops queue surface.
func (h *adminHandlers) listHolds(w http.ResponseWriter, r *http.Request) {
	caseType := compliance.CaseType(r.URL.Query().Get("case_type"))
	if caseType == "" {
		caseType = compliance.CaseTypeKYB
	}
	holds, err := h.compliance.ListHolds(r.Context(), caseType)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, holds)
}

// POST /admin/compliance/holds/{caseID}/resolve {"approved": true, "reason": "..."} — step 3.
func (h *adminHandlers) resolveHold(w http.ResponseWriter, r *http.Request) {
	caseID, err := uuid.Parse(chi.URLParam(r, "caseID"))
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
		Approved bool   `json:"approved"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	c, err := h.compliance.ResolveHold(r.Context(), caseID, adminID, req.Approved, req.Reason)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}

	// Bridge: an approved KYB case activates its tenant. A direct
	// synchronous call, not an outbox event — onboarding approval is a
	// blocking critical-path operation (ARCHITECTURE.md's monolith design
	// reserves direct calls for exactly this), and no module publishes
	// real domain events yet regardless.
	if c.CaseType == compliance.CaseTypeKYB && c.Status == compliance.StatusApproved {
		if err := h.tenant.ApproveKYB(r.Context(), c.ReferenceID); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("case resolved but failed to activate tenant: %w", err))
			return
		}
	}

	writeJSON(w, http.StatusOK, c)
}

// POST /admin/tenants/{tenantID}/api-keys — step 4, credential issuance.
// Refused by tenant.IssueAPIKey unless the tenant is already active (i.e.
// KYB-approved) — nothing is issued before KYB clears.
func (h *adminHandlers) issueAPIKey(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	apiKey, hmacSecret, err := h.tenant.IssueAPIKey(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Shown once — the raw secret is never retrievable again.
	writeJSON(w, http.StatusCreated, map[string]string{"api_key": apiKey, "hmac_secret": hmacSecret})
}

// POST /admin/tenants/{tenantID}/corridors/{corridorID} {"active": true, "fee_bps_override": null} — step 5.
func (h *adminHandlers) setCorridorEntitlement(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	corridorID, err := uuid.Parse(chi.URLParam(r, "corridorID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	var req struct {
		Active         bool `json:"active"`
		FeeBpsOverride *int `json:"fee_bps_override"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	if err := h.tenant.SetCorridorEntitlement(r.Context(), tenantID, corridorID, req.Active, req.FeeBpsOverride); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"active": req.Active})
}

// POST /admin/tenants/{tenantID}/webhook {"url": "https://..."} — step 6, SSRF-validated.
func (h *adminHandlers) setWebhookURL(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	signingSecret, err := h.tenant.SetWebhookURL(r.Context(), tenantID, req.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	resp := map[string]string{"webhook_url": req.URL}
	if signingSecret != "" {
		resp["webhook_signing_secret"] = signingSecret
	}
	writeJSON(w, http.StatusOK, resp)
}
