package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

// buildRouter wires every module built so far into one router. Separated
// from main() so tests can exercise the real HTTP surface directly via
// httptest, without needing a running process. Stores are built once by
// buildStores and shared with main()'s background jobs — see appStores'
// doc comment for why that sharing matters.
func buildRouter(cfg *config.Config, stores *appStores) (chi.Router, error) {
	// stores.tenant satisfies gateway.CredentialLookup — proven at compile
	// time in internal/tenant/tenant.go, used here for real.
	r, protected := gateway.NewRouter(stores.tenant, cfg.HMACClockSkew, stores.audit)

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
	ph := &portalHandlers{
		tenant:       stores.tenant,
		compliance:   stores.compliance,
		session:      stores.session,
		settlement:   stores.settlement,
		notification: stores.notification,
		ledger:       stores.ledger,
		sandboxMode:  cfg.SandboxMode,
	}

	r.Post("/admin/login", h.login)

	// Tenant self-service: passwordless (email magic link), same
	// unauthenticated tier as /admin/login for the three entry points.
	r.Post("/portal/register", ph.register)
	r.Post("/portal/login", ph.login)
	r.Post("/portal/verify", ph.verify)

	r.Route("/portal", func(portal chi.Router) {
		portal.Use(tenant.PortalMiddleware(stores.tenant))

		portal.Get("/me", ph.me)
		portal.Post("/logout", ph.logout)
		portal.Post("/logout-all", ph.logoutAll)
		portal.Post("/delete-account", ph.deleteAccount)
		portal.Post("/kyb", ph.submitKYB)
		portal.Get("/sessions", ph.listSessions)
		portal.Get("/sessions/{sessionID}", ph.getSession)
		portal.Get("/settlements", ph.listSettlements)
		portal.Get("/balance", ph.getBalance)
		portal.Get("/notifications", ph.listNotifications)
		portal.Post("/api-keys", ph.issueAPIKey)
		portal.Get("/api-keys", ph.listAPIKeys)
		portal.Post("/api-keys/{keyID}/revoke", ph.revokeAPIKey)
		portal.Put("/webhook", ph.setWebhookURL)
	})

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
		admin.Post("/tenants/{tenantID}/restore", h.restoreTenant)
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
	// Wired before Mount so it runs first, ahead of every auth middleware
	// (HMAC, admin, portal) — see methodNotAllowedMiddleware's doc comment
	// for why that ordering matters, not just where it's convenient.
	versioned.Use(methodNotAllowedMiddleware(versioned))
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

// httpMethods is every verb this app registers routes under — used to probe
// "does this path exist for some *other* method" below.
var httpMethods = []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// methodNotAllowedMiddleware answers a wrong-method request to a real,
// registered path with a proper 405 (Allow header + JSON body matching
// writeErr's shape everywhere else in this API) instead of letting it fall
// through.
//
// Falling through is the actual bug this fixes: gateway.NewRouter mounts
// its HMAC-protected sub-router at "/" (protecting every tenant-gateway
// route not otherwise made public), and chi middleware runs for *any*
// request that lands on a mounted router regardless of whether that
// request's path/method combo matches anything inside it. So today, e.g. a
// GET to the POST-only /v2/portal/register never reaches chi's own routing
// logic at all — HMACMiddleware fires first, finds no signature headers,
// and returns a misleading 401 ("missing signature headers") instead of an
// honest 405. Every admin/portal sub-route has the identical issue via
// adminauth.Middleware/tenant.PortalMiddleware. Router.Match performs the
// same tree walk as real routing but never invokes a handler or middleware,
// so it's unaffected by any of that — this runs as the very first
// middleware on the top-level router, ahead of every auth layer, precisely
// so it decides "wrong method" before any of them get a chance to
// misreport it as "wrong credentials".
func methodNotAllowedMiddleware(router chi.Router) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if router.Match(chi.NewRouteContext(), r.Method, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			var allowed []string
			for _, method := range httpMethods {
				if method == r.Method {
					continue
				}
				if router.Match(chi.NewRouteContext(), method, r.URL.Path) {
					allowed = append(allowed, method)
				}
			}
			if len(allowed) == 0 {
				// Doesn't exist for any method either — an honest 404, not
				// this middleware's job to report.
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed on %s", r.Method, r.URL.Path))
		})
	}
}
