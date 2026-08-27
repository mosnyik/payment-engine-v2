package gateway

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/sirfi/payment-engine-v2/internal/platform/audit"
	"github.com/sirfi/payment-engine-v2/internal/platform/ratelimit"
)

// NewRouter returns the base router and the protected sub-router, gated by
// HMACMiddleware (or, once tenant config supports it in Phase 2, mTLS).
// Callers mount tenant-facing routes on the returned `protected` router as
// those modules land — /sessions, etc. — without needing to know anything
// about the auth wiring itself. API versioning (/v2) is applied once, at
// the top level, by cmd/server — not baked into this router. limiter is
// nil-safe (see HMACMiddleware's doc comment) — wired here so it's applied
// uniformly to every protected route rather than each call site remembering
// to add it. isProduction gates the HSTS header (ISP §6: "HSTS in
// production") — meaningless over the plain HTTP this binary itself speaks
// in dev, where the reverse-proxy TLS termination ISP §6 describes doesn't
// exist.
//
// mTLS is intentionally not implemented yet: it needs a tenant record to
// say which tenants use it and to pin their certs, and that config doesn't
// exist until the tenant module (Phase 2) is built. HMAC is fully usable
// standalone in the meantime — per-tenant choice of auth method comes once
// tenant exists to express it, without changing this router's shape.
func NewRouter(lookup CredentialLookup, hmacClockSkew time.Duration, auditLogger *audit.Logger, limiter *ratelimit.Limiter, isProduction bool) (router chi.Router, protected chi.Router) {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// Wired before (outside) Recoverer deliberately — see audit.Middleware's
	// doc comment for why a panic would otherwise never get logged.
	r.Use(audit.Middleware(auditLogger))
	r.Use(middleware.Recoverer)
	r.Use(securityHeadersMiddleware(isProduction))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":    "ok",
			"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	})

	protected = chi.NewRouter()
	protected.Use(HMACMiddleware(lookup, hmacClockSkew, limiter))
	r.Mount("/", protected)

	return r, protected
}

// securityHeadersMiddleware sets ISP §6's fixed set of headers on every
// response: X-Content-Type-Options, X-Frame-Options, a restrictive CSP (this
// is a pure JSON API — nothing here ever needs to load a script, style,
// frame, or subresource of its own), no-cache directives, and — in
// production only — HSTS. Deliberately wired inside NewRouter's own `r`
// rather than something cmd/server applies itself, so it's automatic for
// every route mounted here and for any future one, with no per-route
// opt-in to forget. It does NOT cover cmd/server's dev-only /docs route
// (Swagger UI, which loads its JS/CSS from a CDN and would break under this
// CSP) — that's mounted as a sibling of this router, never under it, and is
// skipped entirely in production anyway. X-Powered-By is never set by
// Go's net/http in the first place, so there's nothing to remove.
func securityHeadersMiddleware(isProduction bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
			h.Set("Pragma", "no-cache")
			if isProduction {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
