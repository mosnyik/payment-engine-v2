package main

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

// buildRouter wires every module built so far into one router. Separated
// from main() so tests can exercise the real HTTP surface directly via
// httptest, without needing a running process.
func buildRouter(cfg *config.Config, pool *db.Pool) (chi.Router, error) {
	tenantStore, err := tenant.New(pool, cfg.TenantSecretEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("build router: %w", err)
	}
	complianceStore := compliance.New(pool, compliance.NewRegistry())
	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)

	// tenantStore satisfies gateway.CredentialLookup — proven at compile
	// time in internal/tenant/tenant.go, used here for real.
	r, protected := gateway.NewRouter(tenantStore, cfg.HMACClockSkew)

	// A minimal authenticated smoke-test route — no real tenant-facing
	// business endpoints exist yet (session creation etc. is Phase 5). This
	// is what proves an issued credential actually works end to end, not
	// just that Store-level HMAC logic is correct in isolation.
	protected.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		tenantID, _ := gateway.TenantIDFromContext(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"tenant_id": tenantID.String()})
	})

	h := &adminHandlers{tenant: tenantStore, compliance: complianceStore, admin: adminStore}

	r.Post("/admin/login", h.login)

	r.Route("/admin", func(admin chi.Router) {
		admin.Use(adminauth.Middleware(adminStore))

		admin.Post("/tenants", h.createTenant)
		admin.Post("/tenants/{tenantID}/kyb", h.submitKYB)
		admin.Get("/compliance/holds", h.listHolds)
		admin.Post("/compliance/holds/{caseID}/resolve", h.resolveHold)
		admin.Post("/tenants/{tenantID}/api-keys", h.issueAPIKey)
		admin.Post("/tenants/{tenantID}/corridors/{corridorID}", h.setCorridorEntitlement)
		admin.Post("/tenants/{tenantID}/webhook", h.setWebhookURL)
	})

	return r, nil
}
