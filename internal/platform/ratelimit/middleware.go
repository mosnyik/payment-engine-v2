package ratelimit

import (
	"encoding/json"
	"net/http"

	"github.com/sirfi/payment-engine-v2/internal/platform/audit"
)

// IPMiddleware rejects with 429 once the client IP (audit.ClientIP) exceeds
// limit requests per Limiter's window. Intended for the handful of fully
// public, unauthenticated routes (POST /admin/login, /portal/register,
// /portal/login, /portal/verify) — gateway.HMACMiddleware's own per-tenant-
// tier limiting doesn't apply pre-auth, since there's no tenant/tier to key
// on yet, and these are exactly the "first fully public, unauthenticated,
// non-webhook routes this codebase has" per docs/IMPLEMENTATION_PLAN.md.
//
// group namespaces the limiter key (group+":"+ip) so distinct routes with
// their own limit (e.g. a strict one on /admin/login vs. a looser one on a
// public read endpoint) never share one IP's budget just because they share
// a Limiter instance.
func IPMiddleware(limiter *Limiter, limit int, group string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !limiter.Allow(group+":"+audit.ClientIP(r), limit) {
				writeRateLimitError(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeRateLimitError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
}
