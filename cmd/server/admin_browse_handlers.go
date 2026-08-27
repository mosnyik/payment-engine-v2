package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/audit"
	"github.com/sirfi/payment-engine-v2/internal/session"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

// adminBrowseHandlers implements Phase 11's admin read/browse surface — the
// GET-only counterpart to adminHandlers' act-on-one-record routes, needed
// before any frontend dashboard can be built against this API (see
// docs/IMPLEMENTATION_PLAN.md's Phase 11 section for why this was missing).
// Kept as its own struct/file rather than folded into adminHandlers since
// it depends on two stores (corridor, the request-audit Logger) none of
// adminHandlers' existing routes touch.
type adminBrowseHandlers struct {
	tenant        *tenant.Store
	corridor      *corridor.Store
	session       *session.Store
	admin         *adminauth.Store
	requestAudit  *audit.Logger
}

// defaultPageLimit/maxPageLimit bound every new paginated endpoint below —
// a fixed convention (like session.SessionTTL or settlement.MaxAutoRetryAttempts)
// rather than an ops knob, since there's no per-environment reason to want a
// different default page size.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// parsePagination reads ?limit=&offset= off the query string, clamping limit
// to (0, maxPageLimit] and defaulting to defaultPageLimit — the one place
// every new list handler below gets this behavior from, instead of each
// re-implementing its own bounds checking.
func parsePagination(r *http.Request) (limit, offset int) {
	limit = defaultPageLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}

	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

// paginatedResponse is the consistent envelope every new list endpoint in
// this file returns — items plus enough to page through the rest, since
// (unlike the existing bare-array list endpoints: compliance holds,
// notification dead-letters, ledger discrepancies, all small self-clearing
// operational queues) tenants/corridors/sessions/audit-log rows accumulate
// forever.
type paginatedResponse[T any] struct {
	Items  []T `json:"items"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

func writePaginated[T any](w http.ResponseWriter, items []T, limit, offset, total int) {
	if items == nil {
		items = []T{}
	}
	writeJSON(w, http.StatusOK, paginatedResponse[T]{Items: items, Limit: limit, Offset: offset, Total: total})
}

type tenantResponse struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	FeeBps     int       `json:"fee_bps"`
	WebhookURL *string   `json:"webhook_url,omitempty"`
	CreatedAt  string    `json:"created_at"`
}

func toTenantResponse(t *tenant.Tenant) tenantResponse {
	return tenantResponse{
		ID:         t.ID,
		Name:       t.Name,
		Status:     string(t.Status),
		FeeBps:     t.FeeBps,
		WebhookURL: t.WebhookURL,
		CreatedAt:  t.CreatedAt.Format(http.TimeFormat),
	}
}

// GET /admin/tenants?status=&limit=&offset=
func (h *adminBrowseHandlers) listTenants(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r)

	var status *tenant.Status
	if v := r.URL.Query().Get("status"); v != "" {
		s := tenant.Status(v)
		status = &s
	}

	tenants, total, err := h.tenant.ListTenants(r.Context(), status, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]tenantResponse, len(tenants))
	for i := range tenants {
		resp[i] = toTenantResponse(&tenants[i])
	}
	writePaginated(w, resp, limit, offset, total)
}

// GET /admin/tenants/{tenantID}
func (h *adminBrowseHandlers) getTenant(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, err := h.tenant.GetTenant(r.Context(), id)
	if err == tenant.ErrNotFound {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, toTenantResponse(t))
}

type corridorResponse struct {
	ID                        uuid.UUID `json:"id"`
	CryptoAsset               string    `json:"crypto_asset"`
	CryptoNetwork             string    `json:"crypto_network"`
	FiatCurrency              string    `json:"fiat_currency"`
	Active                    bool      `json:"active"`
	MinAmountFiat             *string   `json:"min_amount_fiat,omitempty"`
	MaxAmountFiat             *string   `json:"max_amount_fiat,omitempty"`
	TravelRuleThresholdFiat   *string   `json:"travel_rule_threshold_fiat,omitempty"`
	TravelRuleWindowSeconds   int64     `json:"travel_rule_window_seconds"`
	ComplianceHoldTimeoutSecs int64     `json:"compliance_hold_timeout_seconds"`
	RequiredDestinationFields []string  `json:"required_destination_fields,omitempty"`
}

func toCorridorResponse(c *corridor.Corridor) corridorResponse {
	resp := corridorResponse{
		ID:                        c.ID,
		CryptoAsset:               c.CryptoAsset,
		CryptoNetwork:             c.CryptoNetwork,
		FiatCurrency:              c.FiatCurrency,
		Active:                    c.Active,
		TravelRuleWindowSeconds:   int64(c.TravelRuleWindow.Seconds()),
		ComplianceHoldTimeoutSecs: int64(c.ComplianceHoldTimeout.Seconds()),
		RequiredDestinationFields: c.RequiredDestinationFields,
	}
	if c.MinAmountFiat != nil {
		s := c.MinAmountFiat.String()
		resp.MinAmountFiat = &s
	}
	if c.MaxAmountFiat != nil {
		s := c.MaxAmountFiat.String()
		resp.MaxAmountFiat = &s
	}
	if c.TravelRuleThresholdFiat != nil {
		s := c.TravelRuleThresholdFiat.String()
		resp.TravelRuleThresholdFiat = &s
	}
	return resp
}

// GET /admin/corridors?active=true&limit=&offset=
func (h *adminBrowseHandlers) listCorridors(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r)
	activeOnly := r.URL.Query().Get("active") == "true"

	corridors, total, err := h.corridor.ListCorridors(r.Context(), activeOnly, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]corridorResponse, len(corridors))
	for i := range corridors {
		resp[i] = toCorridorResponse(&corridors[i])
	}
	writePaginated(w, resp, limit, offset, total)
}

// GET /admin/corridors/{corridorID}
func (h *adminBrowseHandlers) getCorridor(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "corridorID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	c, err := h.corridor.GetCorridorByID(r.Context(), id)
	if err == corridor.ErrNotFound {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, toCorridorResponse(c))
}

// adminSessionResponse is the admin-facing session shape — richer than
// session_handlers.go's tenant-facing sessionResponse, which deliberately
// omits tenant_id/compliance_case_id (ARCHITECTURE.md §5: a tenant never
// sees the specific screening reason behind its own hold). Ops
// investigating across tenants legitimately needs both, plus the other
// internal linkage fields a tenant has no reason to see.
type adminSessionResponse struct {
	ID                   uuid.UUID  `json:"id"`
	TenantID             uuid.UUID  `json:"tenant_id"`
	CorridorID            uuid.UUID  `json:"corridor_id"`
	Status               string     `json:"status"`
	FiatCurrency         string     `json:"fiat_currency"`
	FiatAmount           string     `json:"fiat_amount"`
	CryptoAsset          string     `json:"crypto_asset"`
	CryptoNetwork        string     `json:"crypto_network"`
	ComplianceCaseID     *uuid.UUID `json:"compliance_case_id,omitempty"`
	RateLockID           *uuid.UUID `json:"rate_lock_id,omitempty"`
	DepositReservationID *uuid.UUID `json:"deposit_reservation_id,omitempty"`
	SLABreachedAt        *string    `json:"sla_breached_at,omitempty"`
	CreatedAt            string     `json:"created_at"`
	UpdatedAt            string     `json:"updated_at"`
}

func toAdminSessionResponse(s *session.Session) adminSessionResponse {
	resp := adminSessionResponse{
		ID:                   s.ID,
		TenantID:             s.TenantID,
		CorridorID:           s.CorridorID,
		Status:               string(s.Status),
		FiatCurrency:         s.FiatCurrency,
		FiatAmount:           s.FiatAmount.String(),
		CryptoAsset:          s.CryptoAsset,
		CryptoNetwork:        s.CryptoNetwork,
		ComplianceCaseID:     s.ComplianceCaseID,
		RateLockID:           s.RateLockID,
		DepositReservationID: s.DepositReservationID,
		CreatedAt:            s.CreatedAt.Format(http.TimeFormat),
		UpdatedAt:            s.UpdatedAt.Format(http.TimeFormat),
	}
	if s.SLABreachedAt != nil {
		v := s.SLABreachedAt.Format(http.TimeFormat)
		resp.SLABreachedAt = &v
	}
	return resp
}

// GET /admin/tenants/{tenantID}/sessions?limit=&offset=
func (h *adminBrowseHandlers) listTenantSessions(w http.ResponseWriter, r *http.Request) {
	tenantID, err := uuid.Parse(chi.URLParam(r, "tenantID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	limit, offset := parsePagination(r)

	sessions, total, err := h.session.ListSessionsByTenant(r.Context(), tenantID, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]adminSessionResponse, len(sessions))
	for i := range sessions {
		resp[i] = toAdminSessionResponse(&sessions[i])
	}
	writePaginated(w, resp, limit, offset, total)
}

// GET /admin/sessions/{sessionID} — unlike sessionHandlers.getSession
// (tenant-scoped, 404s across tenants), this has no ownership check: an
// admin can look up any session by ID.
func (h *adminBrowseHandlers) getSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "sessionID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	sess, err := h.session.GetSession(r.Context(), id)
	if err == session.ErrNotFound {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, toAdminSessionResponse(sess))
}

type auditLogEntryResponse struct {
	ID        uuid.UUID       `json:"id"`
	AdminID   uuid.UUID       `json:"admin_id"`
	Action    string          `json:"action"`
	Target    *string         `json:"target,omitempty"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt string          `json:"created_at"`
}

// GET /admin/audit-log?admin_id=&limit=&offset= — admin_audit_log's read
// path: the human-readable "who approved what" trail.
func (h *adminBrowseHandlers) listAuditLog(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r)

	var adminID *uuid.UUID
	if v := r.URL.Query().Get("admin_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		adminID = &id
	}

	entries, total, err := h.admin.ListAuditLog(r.Context(), adminID, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]auditLogEntryResponse, len(entries))
	for i, e := range entries {
		resp[i] = auditLogEntryResponse{
			ID: e.ID, AdminID: e.AdminID, Action: e.Action, Target: e.Target,
			Metadata: e.Metadata, CreatedAt: e.CreatedAt.Format(http.TimeFormat),
		}
	}
	writePaginated(w, resp, limit, offset, total)
}

type requestLogEntryResponse struct {
	ID             uuid.UUID  `json:"id"`
	RequestID      string     `json:"request_id"`
	Method         string     `json:"method"`
	Path           string     `json:"path"`
	Action         string     `json:"action"`
	ResourceType   string     `json:"resource_type,omitempty"`
	ResourceID     string     `json:"resource_id,omitempty"`
	ClientIP       string     `json:"client_ip"`
	UserAgent      string     `json:"user_agent"`
	StatusCode     int        `json:"status_code"`
	ResponseTimeMS int64      `json:"response_time_ms"`
	TenantID       *uuid.UUID `json:"tenant_id,omitempty"`
	APIKeyID       *uuid.UUID `json:"api_key_id,omitempty"`
	AdminID        *uuid.UUID `json:"admin_id,omitempty"`
	CreatedAt      string     `json:"created_at"`
}

// GET /admin/request-audit-log?tenant_id=&admin_id=&limit=&offset= —
// request_audit_log's read path (ISP §7's blanket per-request log). Never
// returns body_hash — it's a hash, not a secret, but it's also not useful
// to a human reviewing this trail.
func (h *adminBrowseHandlers) listRequestAuditLog(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePagination(r)

	var filter audit.LogFilter
	if v := r.URL.Query().Get("tenant_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		filter.TenantID = &id
	}
	if v := r.URL.Query().Get("admin_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		filter.AdminID = &id
	}

	entries, total, err := h.requestAudit.List(r.Context(), filter, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]requestLogEntryResponse, len(entries))
	for i, e := range entries {
		resp[i] = requestLogEntryResponse{
			ID: e.ID, RequestID: e.RequestID, Method: e.Method, Path: e.Path, Action: e.Action,
			ResourceType: e.ResourceType, ResourceID: e.ResourceID, ClientIP: e.ClientIP,
			UserAgent: e.UserAgent, StatusCode: e.StatusCode, ResponseTimeMS: e.ResponseTimeMS,
			TenantID: e.TenantID, APIKeyID: e.APIKeyID, AdminID: e.AdminID,
			CreatedAt: e.CreatedAt.Format(http.TimeFormat),
		}
	}
	writePaginated(w, resp, limit, offset, total)
}
