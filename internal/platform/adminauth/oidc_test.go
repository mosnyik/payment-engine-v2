package adminauth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
)

const fakeOIDCClientID = "test-client-id"

// newFakeOIDCServer stands up a minimal in-process OIDC IdP: discovery
// document, JWKS, and a token endpoint that always returns one RS256-signed
// ID token. claims is called once the server's own URL (the issuer) is
// known, so callers can put a correct "iss" claim in the token. Good enough
// to exercise adminauth.NewOIDCAuthenticator's discovery call and
// Exchange's real verify path without a live IdP — see the plan's "fake
// OIDC test server" decision.
func newFakeOIDCServer(t *testing.T, claims func(issuer string) map[string]any) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	issuer := ts.URL
	tokenClaims := claims(issuer)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/auth",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	})

	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(w, map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": "test-key",
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(bigEndianExponent(key.PublicKey.E)),
			}},
		})
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		idToken, err := signFakeIDToken(key, tokenClaims)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"id_token":     idToken,
		})
	})

	return issuer
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func bigEndianExponent(e int) []byte {
	// Standard RSA public exponent (65537) fits in 3 bytes.
	return []byte{byte(e >> 16), byte(e >> 8), byte(e)}
}

func signFakeIDToken(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test-key"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	b64 := base64.RawURLEncoding
	signingInput := b64.EncodeToString(headerJSON) + "." + b64.EncodeToString(claimsJSON)

	h := crypto.SHA256.New()
	h.Write([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h.Sum(nil))
	if err != nil {
		return "", err
	}

	return signingInput + "." + b64.EncodeToString(sig), nil
}

// defaultFakeClaims is a valid, freshly-issued ID token's claim set for
// fakeOIDCClientID — individual tests mutate the result to exercise one
// failure mode at a time.
func defaultFakeClaims(issuer, nonce string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss":   issuer,
		"sub":   "fake-subject-123",
		"aud":   fakeOIDCClientID,
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"nonce": nonce,
		"email": "admin@sirfi.test",
	}
}

func newTestAuthenticator(t *testing.T, issuer string) *adminauth.OIDCAuthenticator {
	t.Helper()
	auth, err := adminauth.NewOIDCAuthenticator(context.Background(), adminauth.OIDCConfig{
		IssuerURL:    issuer,
		ClientID:     fakeOIDCClientID,
		ClientSecret: "test-client-secret",
		RedirectURL:  "https://admin.sirfi.test/v2/admin/login/oidc/callback",
	})
	if err != nil {
		t.Fatalf("new oidc authenticator: %v", err)
	}
	return auth
}

func TestAuthCodeURLShape(t *testing.T) {
	issuer := newFakeOIDCServer(t, func(issuer string) map[string]any {
		return defaultFakeClaims(issuer, "unused")
	})
	auth := newTestAuthenticator(t, issuer)

	raw := auth.AuthCodeURL("the-state", "the-nonce")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth code url: %v", err)
	}
	if !strings.HasPrefix(raw, issuer) {
		t.Fatalf("expected auth url to point at the issuer %q, got %q", issuer, raw)
	}
	q := u.Query()
	if q.Get("client_id") != fakeOIDCClientID {
		t.Fatalf("expected client_id %q, got %q", fakeOIDCClientID, q.Get("client_id"))
	}
	if q.Get("state") != "the-state" {
		t.Fatalf("expected state %q, got %q", "the-state", q.Get("state"))
	}
	if q.Get("nonce") != "the-nonce" {
		t.Fatalf("expected nonce %q, got %q", "the-nonce", q.Get("nonce"))
	}
	if q.Get("response_type") != "code" {
		t.Fatalf("expected response_type=code, got %q", q.Get("response_type"))
	}
}

func TestExchangeSuccess(t *testing.T) {
	issuer := newFakeOIDCServer(t, func(issuer string) map[string]any {
		return defaultFakeClaims(issuer, "the-nonce")
	})
	auth := newTestAuthenticator(t, issuer)

	subject, email, err := auth.Exchange(context.Background(), "any-code", "the-nonce")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if subject != "fake-subject-123" {
		t.Fatalf("expected subject %q, got %q", "fake-subject-123", subject)
	}
	if email != "admin@sirfi.test" {
		t.Fatalf("expected email %q, got %q", "admin@sirfi.test", email)
	}
}

func TestExchangeNonceMismatch(t *testing.T) {
	issuer := newFakeOIDCServer(t, func(issuer string) map[string]any {
		return defaultFakeClaims(issuer, "the-real-nonce")
	})
	auth := newTestAuthenticator(t, issuer)

	_, _, err := auth.Exchange(context.Background(), "any-code", "a-different-nonce")
	if err != adminauth.ErrOIDCNonceMismatch {
		t.Fatalf("expected ErrOIDCNonceMismatch, got %v", err)
	}
}

func TestExchangeWrongAudienceRejected(t *testing.T) {
	issuer := newFakeOIDCServer(t, func(issuer string) map[string]any {
		claims := defaultFakeClaims(issuer, "the-nonce")
		claims["aud"] = "some-other-client-id"
		return claims
	})
	auth := newTestAuthenticator(t, issuer)

	_, _, err := auth.Exchange(context.Background(), "any-code", "the-nonce")
	if err == nil {
		t.Fatal("expected an error for a token issued to a different audience")
	}
}

func TestExchangeExpiredTokenRejected(t *testing.T) {
	issuer := newFakeOIDCServer(t, func(issuer string) map[string]any {
		claims := defaultFakeClaims(issuer, "the-nonce")
		claims["exp"] = time.Now().Add(-time.Hour).Unix()
		return claims
	})
	auth := newTestAuthenticator(t, issuer)

	_, _, err := auth.Exchange(context.Background(), "any-code", "the-nonce")
	if err == nil {
		t.Fatal("expected an error for an expired token")
	}
}
