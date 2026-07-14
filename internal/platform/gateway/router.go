package gateway

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter returns the base router every module registers its routes on.
// A /healthz route is mounted unauthenticated; everything else modules add
// later goes through HMACMiddleware (or, once tenant config supports it in
// Phase 2, mTLS) via lookup.
//
// mTLS is intentionally not implemented yet: it needs a tenant record to
// say which tenants use it and to pin their certs, and that config doesn't
// exist until the tenant module (Phase 2) is built. HMAC is fully usable
// standalone in the meantime — per-tenant choice of auth method comes once
// tenant exists to express it, without changing this router's shape.
func NewRouter(lookup CredentialLookup) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Group(func(protected chi.Router) {
		protected.Use(HMACMiddleware(lookup))
		// Modules mount their authenticated routes on `protected` as they're built.
	})

	return r
}
