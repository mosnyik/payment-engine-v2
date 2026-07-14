package adminauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type adminIDKey struct{}

// AdminIDFromContext returns the authenticated admin's ID, set by
// Middleware after successful verification.
func AdminIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(adminIDKey{}).(uuid.UUID)
	return id, ok
}

// Middleware verifies the Authorization: Bearer <token> header against the
// admin session store. Entirely separate from the tenant gateway's
// middleware — an admin bearer token is never accepted on tenant routes or
// vice versa.
func Middleware(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(authHeader, "Bearer ")
			if !ok || token == "" {
				writeError(w, errors.New("missing bearer token"))
				return
			}

			adminID, err := store.Authenticate(r.Context(), token)
			if err != nil {
				writeError(w, err)
				return
			}

			ctx := context.WithValue(r.Context(), adminIDKey{}, adminID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
