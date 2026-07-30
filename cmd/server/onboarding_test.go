// Phase 2 acceptance test: drives the full onboarding workflow
// (ARCHITECTURE.md §5) through real HTTP requests against the actual
// router — registration -> KYB submission -> review -> credential
// issuance -> corridor entitlement -> webhook config — proving the
// modules built this phase (tenant, compliance, corridor, adminauth,
// gateway) actually compose, not just that each passes its own tests in
// isolation.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	_ = godotenv.Load("../../.env")

	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	return &config.Config{
		Environment:               "development",
		DatabaseURL:               dbURL,
		TenantSecretEncryptionKey: key,
		HMACClockSkew:             5 * time.Minute,
		AdminSessionTTL:           12 * time.Hour,
		EventbusBatchSize:         50,
		HTTPAddr:                  ":0",
	}
}

// doJSON performs an HTTP request against srv with a JSON body (or nil) and
// decodes the JSON response into out (if non-nil). Fails the test on
// transport errors only — callers assert on status codes themselves.
func doJSON(t *testing.T, client *http.Client, method, url, token string, body any, out any) *http.Response {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response body: %v", err)
		}
	}
	return resp
}

func TestOnboardingWorkflowEndToEnd(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)

	stores, err := buildStores(cfg, pool)
	if err != nil {
		t.Fatalf("build stores: %v", err)
	}
	router, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	client := srv.Client()

	// Out-of-band provisioning: an admin account is created by a script,
	// never via a public HTTP endpoint (see adminauth.CreateAdmin's docs).
	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	adminEmail := "onboarding-test-" + uuid.NewString() + "@sirfi.test"
	if _, err := adminStore.CreateAdmin(ctx, adminEmail, "correct-horse-battery-staple"); err != nil {
		t.Fatalf("provision admin: %v", err)
	}

	// A corridor must already exist for entitlement assignment — set up by
	// ops separately from the onboarding flow (corridor.UpsertCorridor is
	// not exposed as an onboarding HTTP action; see task scoping notes).
	corridorStore := corridor.New(pool)
	corridorID, err := corridorStore.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:   "USDT",
		CryptoNetwork: "TESTNET_ONBOARDING_" + uuid.NewString(),
		FiatCurrency:  "NGN",
		Active:        true,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}

	// --- Step 0: admin login ---
	var loginResp struct {
		Token string `json:"token"`
	}
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/login", "", map[string]string{
		"email": adminEmail, "password": "correct-horse-battery-staple",
	}, &loginResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: expected 200, got %d", resp.StatusCode)
	}
	if loginResp.Token == "" {
		t.Fatal("login: expected a non-empty token")
	}
	token := loginResp.Token

	// Unauthenticated admin requests must be rejected — confirms the
	// middleware is actually wired on the protected group, not just present
	// in the package.
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants", "", map[string]string{"name": "should not work"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated admin request, got %d", resp.StatusCode)
	}

	// --- Step 1: registration ---
	var createResp struct {
		ID string `json:"id"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants", token, map[string]string{
		"name": "Test Fintech " + uuid.NewString(),
	}, &createResp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tenant: expected 201, got %d", resp.StatusCode)
	}
	tenantID, err := uuid.Parse(createResp.ID)
	if err != nil {
		t.Fatalf("parse tenant id: %v", err)
	}

	// Credential issuance before KYB approval must be refused — the
	// onboarding chain's core guarantee.
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/api-keys", token, nil, nil)
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("expected credential issuance to be refused before KYB approval")
	}

	// --- Step 2+3: KYB submission (no provider configured -> hold queue) ---
	var kybCase struct {
		ID     string `json:"ID"`
		Status string `json:"Status"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/kyb", token, map[string]any{
		"submitted_data": map[string]string{"company": "Test Fintech Ltd"},
		"provider_name":  "",
	}, &kybCase)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit kyb: expected 201, got %d", resp.StatusCode)
	}
	if compliance.Status(kybCase.Status) != compliance.StatusHold {
		t.Fatalf("expected kyb case to land in hold, got %s", kybCase.Status)
	}

	// Confirm it shows up in the ops hold queue.
	var holds []struct {
		ID string `json:"ID"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/compliance/holds?case_type=kyb", token, nil, &holds)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list holds: expected 200, got %d", resp.StatusCode)
	}
	foundHold := false
	for _, h := range holds {
		if h.ID == kybCase.ID {
			foundHold = true
		}
	}
	if !foundHold {
		t.Fatal("expected the submitted kyb case to appear in the hold queue")
	}

	// --- Step 3 (cont.): resolve the hold ---
	var resolvedCase struct {
		Status string `json:"Status"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/compliance/holds/"+kybCase.ID+"/resolve", token, map[string]any{
		"approved": true, "reason": "documents verified",
	}, &resolvedCase)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve hold: expected 200, got %d", resp.StatusCode)
	}
	if compliance.Status(resolvedCase.Status) != compliance.StatusApproved {
		t.Fatalf("expected resolved case to be approved, got %s", resolvedCase.Status)
	}

	// --- Step 4: credential issuance (now that KYB is approved) ---
	var credsResp struct {
		APIKey     string `json:"api_key"`
		HMACSecret string `json:"hmac_secret"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/api-keys", token, nil, &credsResp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue api key: expected 201, got %d", resp.StatusCode)
	}
	if credsResp.APIKey == "" || credsResp.HMACSecret == "" {
		t.Fatal("expected non-empty api key and hmac secret")
	}

	// --- Step 5: corridor entitlement ---
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/corridors/"+corridorID.String(), token, map[string]any{
		"active": true,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set corridor entitlement: expected 200, got %d", resp.StatusCode)
	}

	// --- Step 6: webhook config, SSRF-validated ---
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/webhook", token, map[string]string{
		"url": "https://169.254.169.254/latest/meta-data/",
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected the SSRF webhook attempt to be rejected with 400, got %d", resp.StatusCode)
	}

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/webhook", token, map[string]string{
		"url": "https://example.com/webhooks/sirfi",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set webhook url: expected 200, got %d", resp.StatusCode)
	}

	// --- Final proof: the issued credential actually authenticates against
	// a real protected tenant-facing route (/v2/ping) — a completely
	// separate auth surface from /admin, gated by HMACMiddleware rather
	// than adminauth.Middleware. This is what proves the ISSUED credential
	// is usable end to end, not just that Store-level HMAC logic is
	// correct in isolation (already covered in internal/tenant's own test).
	timestampStr := jsonNumberString(time.Now().UnixMilli())
	sig := gateway.Sign(credsResp.HMACSecret, http.MethodGet, "/v2/ping", timestampStr, nil)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/ping", nil)
	req.Header.Set("X-API-Key", credsResp.APIKey)
	req.Header.Set("X-Timestamp", timestampStr)
	req.Header.Set("X-Signature", sig)
	tenantResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("tenant-authenticated request: %v", err)
	}
	defer tenantResp.Body.Close()
	if tenantResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /v2/ping with a valid credential to return 200, got %d", tenantResp.StatusCode)
	}
	var pingBody struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(tenantResp.Body).Decode(&pingBody); err != nil {
		t.Fatalf("decode ping response: %v", err)
	}
	if pingBody.TenantID != tenantID.String() {
		t.Fatalf("expected /v2/ping to identify tenant %s, got %s", tenantID, pingBody.TenantID)
	}

	// Negative case: the same route with no credentials must be rejected —
	// proves the middleware is actually gating /v2/ping, not that it
	// happens to be open.
	noAuthResp, err := client.Get(srv.URL + "/v2/ping")
	if err != nil {
		t.Fatalf("unauthenticated ping request: %v", err)
	}
	defer noAuthResp.Body.Close()
	if noAuthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for /v2/ping with no credentials, got %d", noAuthResp.StatusCode)
	}

	// An admin bearer token must not work on the tenant surface, and vice
	// versa — the two auth spaces are genuinely separate, not just
	// differently-labeled views of the same thing.
	adminOnPingReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/ping", nil)
	adminOnPingReq.Header.Set("Authorization", "Bearer "+token)
	adminOnPingResp, err := client.Do(adminOnPingReq)
	if err != nil {
		t.Fatalf("admin-token-on-tenant-route request: %v", err)
	}
	defer adminOnPingResp.Body.Close()
	if adminOnPingResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected an admin bearer token to be rejected on /v2/ping, got %d", adminOnPingResp.StatusCode)
	}
}

func jsonNumberString(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
