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
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

const (
	headerAPIKey    = "X-API-Key"
	headerTimestamp = "X-Timestamp"
	headerSignature = "X-Signature"

	// maxClockSkew bounds how old (or how far in the future) a signed
	// request's timestamp may be — this is the replay window. A captured,
	// validly-signed request is only replayable within this window.
	maxClockSkew = 5 * time.Minute
)

var (
	ErrMissingHeaders = errors.New("missing X-API-Key/X-Timestamp/X-Signature headers")
	ErrUnknownAPIKey  = errors.New("unknown api key")
	ErrClockSkew      = errors.New("timestamp outside allowed window")
	ErrBadSignature   = errors.New("signature mismatch")
)

// CredentialLookup resolves an API key to the tenant's HMAC secret. The
// tenant module (Phase 2) implements this against its own storage — gateway
// depends only on this interface, never on tenant directly, so the module
// boundary stays a hard one and this package is fully testable before
// tenant exists.
type CredentialLookup interface {
	LookupHMACSecret(ctx context.Context, apiKey string) (secret string, tenantID uuid.UUID, ok bool, err error)
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
// timestamp must fall within maxClockSkew to bound replay even of a
// correctly-signed request. IP allowlisting, if configured per-tenant, is
// layered on separately as defense-in-depth — it is never the sole gate.
func HMACMiddleware(lookup CredentialLookup) func(http.Handler) http.Handler {
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

			secret, tenantID, ok, err := lookup.LookupHMACSecret(r.Context(), apiKey)
			if err != nil {
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			if !ok {
				writeAuthError(w, ErrUnknownAPIKey)
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

			ctx := context.WithValue(r.Context(), tenantIDKey{}, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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
