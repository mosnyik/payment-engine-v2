// Package gateway is the tenant-facing API auth boundary: HMAC+API key and
// (once a tenant module exists to configure it per-tenant) mTLS.
package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/platform/audit"
	"github.com/sirfi/payment-engine-v2/internal/platform/ratelimit"
)

const (
	headerAPIKey    = "X-API-Key"
	headerTimestamp = "X-Timestamp"
	headerSignature = "X-Signature"
)

var (
	ErrMissingHeaders = errors.New("missing X-API-Key/X-Timestamp/X-Signature headers")
	ErrUnknownAPIKey  = errors.New("unknown api key")
	ErrClockSkew      = errors.New("timestamp outside allowed window")
	ErrBadSignature   = errors.New("signature mismatch")
	ErrIPNotAllowed   = errors.New("source ip not in this key's allowlist")
)

// tierLimits maps a tenant API key's rate_limit_tier to its per-minute
// request budget — ISP §6: "100/1,000/10,000 req/min by tier". An unknown
// or empty tier falls back to "basic", the most conservative tier, rather
// than silently allowing unlimited traffic for a value this package
// doesn't recognize.
var tierLimits = map[string]int{
	"basic":      100,
	"standard":   1000,
	"enterprise": 10000,
}

func limitForTier(tier string) int {
	if limit, ok := tierLimits[tier]; ok {
		return limit
	}
	return tierLimits["basic"]
}

// CredentialLookup resolves an API key to the tenant's HMAC secret, the
// key's own row ID (for audit attribution — see audit.Record.APIKeyID),
// its rate-limit tier, and its optional CIDR allowlist. The tenant module
// (Phase 2) implements this against its own storage — gateway depends only
// on this interface, never on tenant directly, so the module boundary
// stays a hard one and this package is fully testable before tenant
// exists.
type CredentialLookup interface {
	LookupHMACSecret(ctx context.Context, apiKey string) (secret string, tenantID uuid.UUID, apiKeyID uuid.UUID, rateLimitTier string, allowedCIDRs []string, ok bool, err error)
}

type tenantIDKey struct{}

// TenantIDFromContext returns the authenticated tenant's ID set by
// HMACMiddleware after successful verification.
func TenantIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(tenantIDKey{}).(uuid.UUID)
	return id, ok
}

// HMACMiddleware verifies the X-API-Key/X-Timestamp/X-Signature headers.
// The signature covers method + path + timestamp + body-hash — not just the
// body — so a captured signature can't be replayed against a different
// endpoint or method. Comparison is constant-time (hmac.Equal); the
// timestamp must fall within maxClockSkew (config.HMACClockSkew — an
// operational setting, not a compiled-in constant, so it's tunable without
// a redeploy) to bound replay even of a correctly-signed request.
//
// limiter is nil-safe: a nil Limiter (or ratelimit.Limiter.Allow's own
// limit<=0 case) simply skips rate limiting, same optional-dependency
// convention as treasury.Store's bus/tenantWebhooks fields. When set, every
// authenticated request is checked against the key's rate_limit_tier
// (ISP §6) before reaching next.
//
// IP allowlisting, if configured per-tenant (CredentialLookup's
// allowedCIDRs), is checked after signature verification — layered on as
// defense-in-depth, never the sole gate.
func HMACMiddleware(lookup CredentialLookup, maxClockSkew time.Duration, limiter *ratelimit.Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get(headerAPIKey)
			timestampHeader := r.Header.Get(headerTimestamp)
			signature := r.Header.Get(headerSignature)

			if apiKey == "" || timestampHeader == "" || signature == "" {
				writeAuthError(w, ErrMissingHeaders)
				return
			}

			ts, err := strconv.ParseInt(timestampHeader, 10, 64)
			if err != nil {
				writeAuthError(w, ErrMissingHeaders)
				return
			}
			requestTime := time.UnixMilli(ts)
			if skew := time.Since(requestTime); skew > maxClockSkew || skew < -maxClockSkew {
				writeAuthError(w, ErrClockSkew)
				return
			}

			secret, tenantID, apiKeyID, rateLimitTier, allowedCIDRs, ok, err := lookup.LookupHMACSecret(r.Context(), apiKey)
			if err != nil {
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if !ok {
				writeAuthError(w, ErrUnknownAPIKey)
				return
			}

			if len(allowedCIDRs) > 0 && !ipAllowed(audit.ClientIP(r), allowedCIDRs) {
				writeAuthError(w, ErrIPNotAllowed)
				return
			}

			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			expected := computeSignature(secret, r.Method, r.URL.Path, timestampHeader, body)
			if !hmac.Equal([]byte(expected), []byte(signature)) {
				writeAuthError(w, ErrBadSignature)
				return
			}

			if !limiter.Allow("tenant:"+tenantID.String(), limitForTier(rateLimitTier)) {
				writeRateLimitError(w)
				return
			}

			if id, ok := audit.IdentityFromContext(r.Context()); ok {
				id.TenantID = &tenantID
				id.APIKeyID = &apiKeyID
			}

			ctx := context.WithValue(r.Context(), tenantIDKey{}, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ipAllowed reports whether clientIP falls within any of allowedCIDRs. A
// malformed clientIP or CIDR entry is treated as non-matching rather than
// erroring the request — the allowlist is defense-in-depth, not the
// primary gate, and a config typo should fail closed on that one entry, not
// take down the whole check.
func ipAllowed(clientIP string, allowedCIDRs []string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}
	for _, cidr := range allowedCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func writeRateLimitError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded"})
}

// computeSignature is exported indirectly via Sign for client/test use, and
// used internally for verification — one implementation, so signer and
// verifier can never drift apart.
func computeSignature(secret, method, path, timestamp string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	signingString := method + "|" + path + "|" + timestamp + "|" + hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingString))
	return hex.EncodeToString(mac.Sum(nil))
}

// Sign computes the signature a client should send in X-Signature for the
// given request parameters. Exposed for tests and for any internal service
// acting as its own API client.
func Sign(secret, method, path, timestamp string, body []byte) string {
	return computeSignature(secret, method, path, timestamp, body)
}

func writeAuthError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
