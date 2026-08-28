// Phase 13 acceptance test: drives the admin SSO/OIDC login routes
// (docs/IMPLEMENTATION_PLAN.md's Phase 13 section) through real HTTP
// requests against the actual router, backed by a fake in-process OIDC IdP
// (discovery + JWKS + RS256 token endpoint) rather than a live one — same
// "fake OIDC test server" decision as internal/platform/adminauth/oidc_test.go,
// just exercised here at the HTTP-handler layer instead of the Go-API layer.
// Reuses testConfig/doJSON from onboarding_test.go (same package).
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

const adminOIDCTestClientID = "test-admin-client"

// newFakeOIDCIdP stands up a minimal in-process OIDC IdP for this test —
// discovery document, JWKS, and a token endpoint that always signs an ID
// token for (subject, email) using whatever nonce setNonce was last called
// with. The indirection exists because the real nonce isn't known until
// *after* the app's own /admin/login/oidc handler mints one — the fake IdP
// has to be reachable (for discovery, at app-startup) before that happens,
// so its token response can't bake the nonce in upfront the way subject/
// email can.
func newFakeOIDCIdP(t *testing.T, subject, email string) (issuer string, setNonce func(string)) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	var nonce string

	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	issuer = ts.URL

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
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
		writeTestJSON(w, map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA",
				"kid": "test-key",
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString([]byte{byte(key.PublicKey.E >> 16), byte(key.PublicKey.E >> 8), byte(key.PublicKey.E)}),
			}},
		})
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		claims := map[string]any{
			"iss":   issuer,
			"sub":   subject,
			"aud":   adminOIDCTestClientID,
			"exp":   now.Add(time.Hour).Unix(),
			"iat":   now.Unix(),
			"nonce": nonce,
			"email": email,
		}
		idToken, err := signTestIDToken(key, claims)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeTestJSON(w, map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"id_token":     idToken,
		})
	})

	return issuer, func(n string) { nonce = n }
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func signTestIDToken(key *rsa.PrivateKey, claims map[string]any) (string, error) {
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

// adminOIDCTestServer bundles everything one test needs: a running app
// server wired to a fake IdP for (subject, email), a redirect-preserving
// HTTP client, the app config, and a way to tell the fake IdP what nonce to
// sign into its next ID token.
type adminOIDCTestServer struct {
	srv      *httptest.Server
	client   *http.Client
	cfg      *config.Config
	pool     *db.Pool
	setNonce func(string)
}

func buildAdminOIDCTestServer(t *testing.T, subject, email string) *adminOIDCTestServer {
	t.Helper()

	issuer, setNonce := newFakeOIDCIdP(t, subject, email)

	cfg := testConfig(t)
	cfg.AdminOIDC = config.AdminOIDCConfig{
		IssuerURL:    issuer,
		ClientID:     adminOIDCTestClientID,
		ClientSecret: "test-client-secret",
		RedirectURL:  "http://localhost/v2/admin/login/oidc/callback",
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)

	stores, err := buildStores(ctx, cfg, pool)
	if err != nil {
		t.Fatalf("build stores: %v", err)
	}
	if stores.adminOIDC == nil {
		t.Fatal("expected stores.adminOIDC to be wired when ADMIN_OIDC_ISSUER_URL is set")
	}

	router, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// Doesn't follow the 302 from /admin/login/oidc — the test only needs
	// the Set-Cookie headers and never actually talks to the fake IdP's
	// (unimplemented) /auth endpoint, since the real exchange happens
	// entirely inside the app's own callback handler.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &adminOIDCTestServer{srv: srv, client: client, cfg: cfg, pool: pool, setNonce: setNonce}
}

// startLogin hits GET /admin/login/oidc and returns the state/nonce cookie
// values the handler minted, for the test to round-trip into the callback
// request (and to feed the real nonce to the fake IdP via setNonce).
func (a *adminOIDCTestServer) startLogin(t *testing.T) (state, nonce string) {
	t.Helper()
	resp, err := a.client.Get(a.srv.URL + "/v2/admin/login/oidc")
	if err != nil {
		t.Fatalf("start oidc login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 from /admin/login/oidc, got %d", resp.StatusCode)
	}

	for _, c := range resp.Cookies() {
		switch c.Name {
		case "admin_oidc_state":
			state = c.Value
		case "admin_oidc_nonce":
			nonce = c.Value
		}
	}
	if state == "" || nonce == "" {
		t.Fatalf("expected both state and nonce cookies to be set, got state=%q nonce=%q", state, nonce)
	}
	return state, nonce
}

// callback builds and sends the GET /admin/login/oidc/callback request
// with the given query state and cookie state/nonce (deliberately separate
// parameters so tests can mismatch them on purpose).
func (a *adminOIDCTestServer) callback(t *testing.T, queryState, cookieState, cookieNonce string, withCookies bool) *http.Response {
	t.Helper()
	callbackURL := a.srv.URL + "/v2/admin/login/oidc/callback?code=any-code&state=" + queryState
	req, err := http.NewRequest(http.MethodGet, callbackURL, nil)
	if err != nil {
		t.Fatalf("build callback request: %v", err)
	}
	if withCookies {
		req.AddCookie(&http.Cookie{Name: "admin_oidc_state", Value: cookieState})
		req.AddCookie(&http.Cookie{Name: "admin_oidc_nonce", Value: cookieNonce})
	}
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	return resp
}

func TestAdminOIDCLoginHappyPath(t *testing.T) {
	adminEmail := "oidc-test-" + uuid.NewString() + "@sirfi.test"
	subject := "sub-" + uuid.NewString()

	a := buildAdminOIDCTestServer(t, subject, adminEmail)
	adminStore := adminauth.New(a.pool, a.cfg.AdminSessionTTL)
	if _, err := adminStore.CreateAdmin(context.Background(), adminEmail, "some-password-never-used"); err != nil {
		t.Fatalf("provision admin: %v", err)
	}

	state, nonce := a.startLogin(t)
	a.setNonce(nonce)

	resp := a.callback(t, state, state, nonce, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from callback, got %d", resp.StatusCode)
	}

	var loginResp struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		t.Fatalf("decode callback response: %v", err)
	}
	if loginResp.Token == "" {
		t.Fatal("expected a non-empty token")
	}

	// Prove the token actually works on a real authenticated admin route,
	// not just that LoginWithOIDC returned something.
	req, err := http.NewRequest(http.MethodGet, a.srv.URL+"/v2/admin/tenants", nil)
	if err != nil {
		t.Fatalf("build tenants request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+loginResp.Token)
	tenantsResp, err := a.client.Do(req)
	if err != nil {
		t.Fatalf("tenants request: %v", err)
	}
	defer tenantsResp.Body.Close()
	if tenantsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected the OIDC-issued token to authenticate GET /admin/tenants, got %d", tenantsResp.StatusCode)
	}
}

func TestAdminOIDCLoginUnprovisionedEmailRejected(t *testing.T) {
	adminEmail := "oidc-nobody-" + uuid.NewString() + "@sirfi.test"
	subject := "sub-" + uuid.NewString()

	a := buildAdminOIDCTestServer(t, subject, adminEmail)
	// Deliberately never provisioned via adminctl/CreateAdmin — invite-only
	// should reject this even though the ID token itself verifies fine.

	state, nonce := a.startLogin(t)
	a.setNonce(nonce)

	resp := a.callback(t, state, state, nonce, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unprovisioned email, got %d", resp.StatusCode)
	}
}

func TestAdminOIDCLoginStateMismatchRejected(t *testing.T) {
	adminEmail := "oidc-test-" + uuid.NewString() + "@sirfi.test"
	subject := "sub-" + uuid.NewString()

	a := buildAdminOIDCTestServer(t, subject, adminEmail)
	adminStore := adminauth.New(a.pool, a.cfg.AdminSessionTTL)
	if _, err := adminStore.CreateAdmin(context.Background(), adminEmail, "some-password-never-used"); err != nil {
		t.Fatalf("provision admin: %v", err)
	}

	state, nonce := a.startLogin(t)
	a.setNonce(nonce)

	resp := a.callback(t, "a-different-state-entirely", state, nonce, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a state mismatch, got %d", resp.StatusCode)
	}
}

func TestAdminOIDCLoginMissingCookiesRejected(t *testing.T) {
	adminEmail := "oidc-test-" + uuid.NewString() + "@sirfi.test"
	subject := "sub-" + uuid.NewString()

	a := buildAdminOIDCTestServer(t, subject, adminEmail)

	// Never called startLogin, so there's no state/nonce cookie pair —
	// simulates a callback hit without a real prior login attempt.
	resp := a.callback(t, "whatever-state", "", "", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing login-state cookies, got %d", resp.StatusCode)
	}
}
