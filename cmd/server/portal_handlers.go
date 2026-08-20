package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/notification"
	"github.com/sirfi/payment-engine-v2/internal/session"
	"github.com/sirfi/payment-engine-v2/internal/settlement"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

// portalHandlers implements the tenant self-service surface: passwordless
// registration/login (email magic link) plus the dashboard's read/write
// routes. Every handler except register/login/verify sits behind
// tenant.PortalMiddleware — a completely separate credential space from
// both the tenant gateway's HMAC auth (machine-to-machine) and adminauth
// (internal staff).
type portalHandlers struct {
	tenant       *tenant.Store
	compliance   *compliance.Store
	session      *session.Store
	settlement   *settlement.Store
	notification *notification.Store
	ledger       *ledger.Ledger
	sandboxMode  bool
}

func portalTenantID(r *http.Request) (uuid.UUID, bool) {
	return tenant.PortalTenantIDFromContext(r.Context())
}

// POST /v2/portal/register {"name": "...", "email": "..."} — creates the
// pending_kyb tenant and sends a magic link. No session token is returned
// here: registering doesn't prove email ownership, verifying the link does.
func (h *portalHandlers) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Email == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name and email are required"))
		return
	}

	_, err := h.tenant.RegisterTenant(r.Context(), req.Name, req.Email)
	if errors.Is(err, tenant.ErrEmailTaken) {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// Best-effort: the tenant record already exists regardless of whether
	// the email actually sends — a delivery failure here (e.g. no email
	// provider configured) shouldn't fail a registration that already
	// succeeded. The tenant can always retry via login.
	if err := h.tenant.RequestMagicLink(r.Context(), req.Email); err != nil {
		log.Printf("portal: register: send magic link: %v", err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message": "check your email for a login link"})
}

// POST /v2/portal/login {"email": "..."} — always responds identically
// regardless of whether the email is registered (RequestMagicLink's
// ErrNotFound is deliberately swallowed here), so this endpoint can't be
// used to enumerate registered emails.
func (h *portalHandlers) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := h.tenant.RequestMagicLink(r.Context(), req.Email); err != nil && !errors.Is(err, tenant.ErrNotFound) {
		log.Printf("portal: login: send magic link: %v", err)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"message": "if that email is registered, a login link has been sent"})
}

// POST /v2/portal/verify {"token": "..."} — redeems a magic link for a
// real portal session token.
func (h *portalHandlers) verify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	sessionToken, _, err := h.tenant.VerifyMagicLink(r.Context(), req.Token)
	if errors.Is(err, tenant.ErrInvalidMagicLink) {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": sessionToken})
}

type portalTenantResponse struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	FeeBps     int       `json:"fee_bps"`
	WebhookURL *string   `json:"webhook_url,omitempty"`
	CreatedAt  string    `json:"created_at"`
}

// GET /v2/portal/me
func (h *portalHandlers) me(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	t, err := h.tenant.GetTenant(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, portalTenantResponse{
		ID:         t.ID,
		Name:       t.Name,
		Status:     string(t.Status),
		FeeBps:     t.FeeBps,
		WebhookURL: t.WebhookURL,
		CreatedAt:  t.CreatedAt.Format(http.TimeFormat),
	})
}

// POST /v2/portal/kyb {"submitted_data": {...}} — self-service counterpart
// to adminHandlers.submitKYB. provider_name is deliberately never taken
// from the request: a self-service caller must never be able to pick a
// screening provider or otherwise influence hold-queue routing. Sandbox
// mode's forced sandbox provider still applies (server-side check only).
func (h *portalHandlers) submitKYB(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}

	var req struct {
		SubmittedData json.RawMessage `json:"submitted_data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	providerName := ""
	if h.sandboxMode {
		providerName = compliance.SandboxProviderName
	}

	c, err := h.compliance.ScreenTenant(r.Context(), tenantID, req.SubmittedData, providerName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// Bridge: a case a registered provider approved outright activates its
	// tenant here — same bridge adminHandlers.submitKYB/resolveHold apply.
	if c.CaseType == compliance.CaseTypeKYB && c.Status == compliance.StatusApproved {
		if err := h.tenant.ApproveKYB(r.Context(), c.ReferenceID); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("case approved but failed to activate tenant: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusCreated, c)
}

// GET /v2/portal/sessions
func (h *portalHandlers) listSessions(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	sessions, err := h.session.ListSessionsByTenant(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]sessionResponse, len(sessions))
	for i := range sessions {
		resp[i] = toSessionResponse(&sessions[i])
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /v2/portal/sessions/{sessionID}. Not found both when the session
// truly doesn't exist and when it belongs to a different tenant — never a
// 403 — same ownership-check pattern as sessionHandlers.getSession.
func (h *portalHandlers) getSession(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
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

// GET /v2/portal/settlements?status=
func (h *portalHandlers) listSettlements(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	var statusFilter *settlement.Status
	if raw := r.URL.Query().Get("status"); raw != "" {
		st := settlement.Status(raw)
		statusFilter = &st
	}
	settlements, err := h.settlement.ListSettlementsByTenant(r.Context(), tenantID, statusFilter)
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

// GET /v2/portal/balance?fiat_currency=NGN — the tenant's own
// tenant_payable liability balance for that currency.
func (h *portalHandlers) getBalance(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	fiatCurrency := r.URL.Query().Get("fiat_currency")
	if fiatCurrency == "" {
		writeErr(w, http.StatusBadRequest, errors.New("fiat_currency query parameter is required"))
		return
	}
	balance, err := h.ledger.GetBalanceForTenant(r.Context(), tenantID, "tenant_payable", fiatCurrency)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"fiat_currency": fiatCurrency, "tenant_payable": balance.String()})
}

// GET /v2/portal/notifications?status= — reuses deliveryResponse/
// toDeliveryResponse from notification_handlers.go.
func (h *portalHandlers) listNotifications(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	var statusFilter *notification.Status
	if raw := r.URL.Query().Get("status"); raw != "" {
		st := notification.Status(raw)
		statusFilter = &st
	}
	deliveries, err := h.notification.ListDeliveriesByTenant(r.Context(), tenantID, statusFilter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]deliveryResponse, len(deliveries))
	for i := range deliveries {
		resp[i] = toDeliveryResponse(&deliveries[i])
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v2/portal/api-keys — self-service counterpart to
// adminHandlers.issueAPIKey. Shown once, same as the admin path.
func (h *portalHandlers) issueAPIKey(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	apiKey, hmacSecret, err := h.tenant.IssueAPIKey(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"api_key": apiKey, "hmac_secret": hmacSecret})
}

type apiKeyResponse struct {
	ID        uuid.UUID `json:"id"`
	APIKey    string    `json:"api_key"`
	Active    bool      `json:"active"`
	CreatedAt string    `json:"created_at"`
	RevokedAt *string   `json:"revoked_at,omitempty"`
}

// GET /v2/portal/api-keys — never includes the HMAC secret, shown once at
// issuance and never again.
func (h *portalHandlers) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	keys, err := h.tenant.ListAPIKeys(r.Context(), tenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]apiKeyResponse, len(keys))
	for i, k := range keys {
		var revokedAt *string
		if k.RevokedAt != nil {
			s := k.RevokedAt.Format(http.TimeFormat)
			revokedAt = &s
		}
		resp[i] = apiKeyResponse{
			ID:        k.ID,
			APIKey:    k.APIKey,
			Active:    k.Active,
			CreatedAt: k.CreatedAt.Format(http.TimeFormat),
			RevokedAt: revokedAt,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v2/portal/api-keys/{keyID}/revoke
func (h *portalHandlers) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
		return
	}
	keyID, err := uuid.Parse(chi.URLParam(r, "keyID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	err = h.tenant.RevokeAPIKey(r.Context(), tenantID, keyID)
	if errors.Is(err, tenant.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// PUT /v2/portal/webhook {"url": "https://..."} — self-service counterpart
// to adminHandlers.setWebhookURL, same SSRF validation and wire shape.
func (h *portalHandlers) setWebhookURL(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := portalTenantID(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, errors.New("missing tenant identity"))
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
