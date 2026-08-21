package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Middleware records one Record per request via logger.Log — enqueued, not
// written inline, so the database is off the response's critical path. It
// must be wired outside (before) Recoverer, not after: a panic unwinds past
// any middleware between it and Recoverer without running that middleware's
// post-next() code, so an inner audit middleware would silently never log
// the requests that paniced into a 500 — exactly the ones most worth
// logging. See cmd/server's router wiring for the resulting order.
func Middleware(logger *Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/health") {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()

			var bodyHash string
			if r.Body != nil {
				body, err := io.ReadAll(r.Body)
				if err == nil {
					sum := sha256.Sum256(body)
					bodyHash = hex.EncodeToString(sum[:])
					r.Body = io.NopCloser(bytes.NewReader(body))
				}
			}

			ctx, identity := newIdentityContext(r.Context())
			r = r.WithContext(ctx)

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			routePattern := ""
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				routePattern = rctx.RoutePattern()
			}
			resourceType, resourceID := deriveResource(r, routePattern)

			logger.Log(Record{
				RequestID:      middleware.GetReqID(r.Context()),
				Method:         r.Method,
				Path:           r.URL.Path,
				Action:         r.Method + " " + firstNonEmpty(routePattern, r.URL.Path),
				ResourceType:   resourceType,
				ResourceID:     resourceID,
				ClientIP:       clientIP(r),
				UserAgent:      r.UserAgent(),
				BodyHash:       bodyHash,
				StatusCode:     ww.Status(),
				ResponseTimeMS: time.Since(start).Milliseconds(),
				TenantID:       identity.TenantID,
				APIKeyID:       identity.APIKeyID,
				AdminID:        identity.AdminID,
			})
		})
	}
}

// deriveResource is a heuristic, not a registry: it reads the resource type
// off the path segment immediately before the route's first {param}, and
// the resource ID off that param's value — e.g.
// "/admin/tenants/{tenantID}/kyb" -> ("tenants", "<the actual UUID>"). A
// route with no path param (e.g. "/portal/register") falls back to its last
// static segment as the resource type, with no ID.
func deriveResource(r *http.Request, routePattern string) (resourceType, resourceID string) {
	segments := strings.Split(strings.Trim(routePattern, "/"), "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			if i > 0 {
				resourceType = segments[i-1]
			}
			paramName := strings.Trim(seg, "{}")
			return resourceType, chi.URLParam(r, paramName)
		}
	}
	if n := len(segments); n > 0 && segments[n-1] != "" {
		return segments[n-1], ""
	}
	return "", ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// clientIP prefers X-Forwarded-For, since production sits behind the
// reverse proxy documented in ISP §6, and falls back to the direct
// connection for local/dev use.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if idx := strings.Index(fwd, ","); idx != -1 {
			return strings.TrimSpace(fwd[:idx])
		}
		return strings.TrimSpace(fwd)
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return real
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
