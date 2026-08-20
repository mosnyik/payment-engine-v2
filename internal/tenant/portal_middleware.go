package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type portalTenantIDKey struct{}

// PortalTenantIDFromContext returns the authenticated tenant's ID, set by
// PortalMiddleware after successful verification. Distinct from
// gateway.TenantIDFromContext — a browser portal session token and an
// HMAC-signed API credential are never conflated in code, even though both
// ultimately resolve to a tenant ID.
func PortalTenantIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(portalTenantIDKey{}).(uuid.UUID)
	return id, ok
}

// PortalMiddleware verifies the Authorization: Bearer <token> header
// against the tenant_sessions store. Mirrors adminauth.Middleware exactly,
// on a completely separate credential space — a portal session token is
// never accepted on gateway (HMAC) or admin routes, or vice versa.
func PortalMiddleware(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(authHeader, "Bearer ")
			if !ok || token == "" {
				writePortalAuthError(w, errors.New("missing bearer token"))
				return
			}

			tenantID, err := store.AuthenticateSession(r.Context(), token)
			if err != nil {
				writePortalAuthError(w, err)
				return
			}

			ctx := context.WithValue(r.Context(), portalTenantIDKey{}, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writePortalAuthError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
