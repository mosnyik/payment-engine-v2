package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/kyc"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
)

// TestKYCVerify drives POST /v2/kyc/verify through the real router, with
// the sandbox provider swapped in so nothing is billed.
func TestKYCVerify(t *testing.T) {
	cfg := testConfig(t)
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
	stores.kyc = kyc.New(pool, kyc.SandboxProvider{}, cfg.TenantSecretEncryptionKey)
	router, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	client := srv.Client()

	tenantID, err := stores.tenant.CreateTenant(ctx, "KYC Builder "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := stores.tenant.ApproveKYB(ctx, tenantID); err != nil {
		t.Fatalf("approve kyb: %v", err)
	}
	issue := func(perms ...string) (string, string) {
		key, secret, err := stores.tenant.IssueAPIKey(ctx, tenantID, perms...)
		if err != nil {
			t.Fatalf("issue api key: %v", err)
		}
		return key, secret
	}
	unscopedKey, unscopedSecret := issue()
	sessionsKey, sessionsSecret := issue(gateway.PermissionSessionsWrite, gateway.PermissionSessionsRead)
	kycKey, kycSecret := issue(gateway.PermissionKYCVerify)

	post := func(key, secret string, body any) (int, map[string]any, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		ts := jsonNumberString(time.Now().UnixMilli())
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/kyc/verify", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", key)
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Signature", gateway.Sign(secret, http.MethodPost, "/v2/kyc/verify", ts, raw))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /v2/kyc/verify: %v", err)
		}
		defer resp.Body.Close()
		text, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(text, &out)
		return resp.StatusCode, out, string(text)
	}
	body := func(idNumber string) map[string]any {
		return map[string]any{
			"id_type": "bvn", "id_number": idNumber, "first_name": "Ada", "last_name": "Obi",
			"date_of_birth": "1990-01-31", "consent": true,
		}
	}

	// --- Explicit permission: unscoped and sessions-only keys are refused ---
	for name, k := range map[string][2]string{"unscoped": {unscopedKey, unscopedSecret}, "sessions-only": {sessionsKey, sessionsSecret}} {
		if code, _, text := post(k[0], k[1], body("22233344455")); code != http.StatusForbidden {
			t.Fatalf("%s key: expected 403, got %d: %s", name, code, text)
		}
	}

	// --- Match ---
	code, out, text := post(kycKey, kycSecret, body("22233344455"))
	if code != http.StatusOK || out["result"] != "match" || out["id_type"] != "bvn" || out["check_id"] == "" {
		t.Fatalf("match: got %d %s", code, text)
	}
	checks, _ := out["checks"].(map[string]any)
	if checks["first_name"] != "match" || checks["last_name"] != "match" || checks["date_of_birth"] != "match" {
		t.Fatalf("match: unexpected checks %s", text)
	}
	for _, leaked := range []string{"22233344455", "1990-01-31", "Ada", "Obi"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("response must not echo personal data (%q): %s", leaked, text)
		}
	}

	// The row is attributed to the tenant and the key that made the call.
	checkID, _ := uuid.Parse(out["check_id"].(string))
	var gotTenant uuid.UUID
	var gotKeyID *uuid.UUID
	var provider string
	if err := pool.QueryRow(ctx, `SELECT tenant_id, api_key_id, provider FROM kyc_checks WHERE id = $1`, checkID).Scan(&gotTenant, &gotKeyID, &provider); err != nil {
		t.Fatalf("load kyc check: %v", err)
	}
	var kycKeyID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM tenant_api_keys WHERE api_key = $1`, kycKey).Scan(&kycKeyID); err != nil {
		t.Fatalf("load key id: %v", err)
	}
	if gotTenant != tenantID || gotKeyID == nil || *gotKeyID != kycKeyID || provider != kyc.SandboxProviderName {
		t.Fatalf("unexpected attribution: tenant=%s key=%v provider=%s", gotTenant, gotKeyID, provider)
	}

	// --- Other sandbox outcomes ---
	for idNumber, want := range map[string]string{"22233344450": "not_found", "22233344451": "no_match", "22233344452": "partial_match"} {
		if code, out, text := post(kycKey, kycSecret, body(idNumber)); code != http.StatusOK || out["result"] != want {
			t.Fatalf("id %s: expected 200 %s, got %d %s", idNumber, want, code, text)
		}
	}
	if code, _, text := post(kycKey, kycSecret, body("22233344459")); code != http.StatusServiceUnavailable || strings.Contains(text, "sandbox") {
		t.Fatalf("provider down: expected a generic 503, got %d %s", code, text)
	}

	// --- Validation ---
	noConsent := body("22233344455")
	noConsent["consent"] = false
	unknownField := body("22233344455")
	unknownField["photo"] = "x"
	badType := body("22233344455")
	badType["id_type"] = "passport"
	for name, b := range map[string]map[string]any{"no consent": noConsent, "unknown field": unknownField, "bad id_type": badType} {
		if code, _, text := post(kycKey, kycSecret, b); code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d %s", name, code, text)
		}
	}

	// --- No provider configured ---
	stores.kyc = kyc.New(pool, nil, cfg.TenantSecretEncryptionKey)
	router2, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	srv2 := httptest.NewServer(router2)
	t.Cleanup(srv2.Close)
	raw, _ := json.Marshal(body("22233344455"))
	ts := jsonNumberString(time.Now().UnixMilli())
	req, _ := http.NewRequest(http.MethodPost, srv2.URL+"/v2/kyc/verify", bytes.NewReader(raw))
	req.Header.Set("X-API-Key", kycKey)
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", gateway.Sign(kycSecret, http.MethodPost, "/v2/kyc/verify", ts, raw))
	resp, err := srv2.Client().Do(req)
	if err != nil {
		t.Fatalf("POST without provider: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no provider: expected 503, got %d", resp.StatusCode)
	}
}
