package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
)

// buildRouter wires every module built so far into one router. Separated
// from main() so tests can exercise the real HTTP surface directly via
// httptest, without needing a running process. Stores are built once by
// buildStores and shared with main()'s background jobs — see appStores'
// doc comment for why that sharing matters.
func buildRouter(cfg *config.Config, stores *appStores) (chi.Router, error) {
	// stores.tenant satisfies gateway.CredentialLookup — proven at compile
	// time in internal/tenant/tenant.go, used here for real.
	r, protected := gateway.NewRouter(stores.tenant, cfg.HMACClockSkew)

	// A minimal authenticated smoke-test route proving an issued credential
	// actually works end to end, not just that Store-level HMAC logic is
	// correct in isolation.
	protected.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		tenantID, _ := gateway.TenantIDFromContext(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"tenant_id": tenantID.String()})
	})

	sh := &sessionHandlers{session: stores.session}
	protected.Post("/sessions", sh.createSession)
	protected.Get("/sessions/{sessionID}", sh.getSession)

	h := &adminHandlers{tenant: stores.tenant, compliance: stores.compliance, admin: stores.admin, session: stores.session, sandboxMode: cfg.SandboxMode}
	sth := &settlementHandlers{settlement: stores.settlement}
	th := &treasuryHandlers{treasury: stores.treasury}
	nh := &notificationHandlers{notification: stores.notification}
	lh := &ledgerHandlers{ledger: stores.ledger}
	rh := &rateHandlers{rate: stores.rate}

	r.Post("/admin/login", h.login)

	// Public, unauthenticated — same tier as /admin/login, deliberately not
	// behind the tenant gateway's HMAC (see rate_handlers.go's doc comment).
	r.Get("/rate/{fiatCurrency}", rh.getRate)

	// Self-verified via settlement.VerifyWebhookSignature, not the tenant
	// gateway's HMAC or adminauth — same unauthenticated tier as
	// /admin/login, since a settlement provider is neither a tenant nor an
	// admin.
	r.Post("/webhooks/settlement/{providerName}", sth.handleWebhook)

	// Same unauthenticated, self-verified tier as the settlement webhook
	// above — self-custody deposit detection already runs via watcher
	// polling (treasury/watcher.go), so this is only Busha's partner-
	// custodied callback path today, hence the static "busha" segment
	// rather than settlement's {providerName}.
	r.Post("/webhooks/treasury/busha", th.handleDepositWebhook)

	r.Route("/admin", func(admin chi.Router) {
		admin.Use(adminauth.Middleware(stores.admin))

		admin.Post("/tenants", h.createTenant)
		admin.Post("/tenants/{tenantID}/kyb", h.submitKYB)
		admin.Get("/compliance/holds", h.listHolds)
		admin.Post("/compliance/holds/{caseID}/resolve", h.resolveHold)
		admin.Post("/tenants/{tenantID}/api-keys", h.issueAPIKey)
		admin.Post("/tenants/{tenantID}/corridors/{corridorID}", h.setCorridorEntitlement)
		admin.Post("/tenants/{tenantID}/webhook", h.setWebhookURL)
		admin.Post("/sessions/{sessionID}/resolve", h.resolveSessionHold)
		admin.Get("/settlements", sth.listSettlements)
		admin.Post("/settlements/{settlementID}/retry", sth.retrySettlement)
		admin.Post("/settlements/reversals/{reversalID}/resolve", sth.resolveReversal)
		admin.Get("/notifications/deliveries", nh.listDeliveries)
		admin.Post("/notifications/deliveries/{deliveryID}/retry", nh.retryDelivery)
		admin.Get("/ledger/discrepancies", lh.listDiscrepancies)
		admin.Post("/ledger/discrepancies/{discrepancyID}/resolve", lh.resolveDiscrepancy)
	})

	// Every route above lives under /v2 — the whole app is versioned at
	// this single mount point rather than per-route.
	versioned := chi.NewRouter()
	versioned.Mount("/v2", r)

	// Dev/staging-only: browse docs/openapi.yaml at /docs. Unversioned
	// (it's tooling, not an API endpoint) and skipped in production since
	// it reads the spec straight off disk rather than embedding it.
	if !cfg.IsProduction() {
		d := docsHandlers{}
		versioned.Get("/docs", d.ui)
		versioned.Get("/docs/openapi.yaml", d.spec)
	}

	return versioned, nil
}
