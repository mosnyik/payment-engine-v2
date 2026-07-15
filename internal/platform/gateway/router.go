package gateway

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter returns the base router and the protected sub-router mounted at
// /v1, gated by HMACMiddleware (or, once tenant config supports it in
// Phase 2, mTLS). Callers mount tenant-facing routes on the returned
// `protected` router as those modules land — /v1/sessions, etc. — without
// needing to know anything about the auth wiring itself.
//
// mTLS is intentionally not implemented yet: it needs a tenant record to
// say which tenants use it and to pin their certs, and that config doesn't
// exist until the tenant module (Phase 2) is built. HMAC is fully usable
// standalone in the meantime — per-tenant choice of auth method comes once
// tenant exists to express it, without changing this router's shape.
func NewRouter(lookup CredentialLookup, hmacClockSkew time.Duration) (router chi.Router, protected chi.Router) {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	protected = chi.NewRouter()
	protected.Use(HMACMiddleware(lookup, hmacClockSkew))
	r.Mount("/v1", protected)

	return r, protected
}
