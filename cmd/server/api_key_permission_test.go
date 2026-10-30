// Phase 9 acceptance test: proves gateway.RequirePermission actually gates
// the real HTTP surface, not just the middleware in isolation (see
// internal/platform/gateway/hmac_test.go for that). session_test.go already
// exercises an unrestricted key against both routes end to end; this only
// needs to prove the scoped-out case, since RequirePermission runs ahead of
// any handler/business logic — a session need not exist, or even a real
// corridor/rate/wallet fixture, for a 403 to occur.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
)

func TestScopedAPIKey_DeniedByRequirePermission(t *testing.T) {
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
	router, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	client := srv.Client()

	tenantID, err := stores.tenant.CreateTenant(ctx, "Permission Test "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := stores.tenant.ApproveKYB(ctx, tenantID); err != nil {
		t.Fatalf("approve kyb: %v", err)
	}

	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	adminEmail := "perm-test-" + uuid.NewString() + "@sirfi.test"
	if _, err := adminStore.CreateAdmin(ctx, adminEmail, "correct-horse-battery-staple"); err != nil {
		t.Fatalf("provision admin: %v", err)
	}
	var loginResp struct {
		Token string `json:"token"`
	}
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/login", "", map[string]string{
		"email": adminEmail, "password": "correct-horse-battery-staple",
	}, &loginResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login: expected 200, got %d", resp.StatusCode)
	}

	issueKey := func(permissions []string) (apiKey, hmacSecret string) {
		var keyResp struct {
			APIKey     string `json:"api_key"`
			HMACSecret string `json:"hmac_secret"`
		}
		resp := doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/api-keys", loginResp.Token,
			map[string]any{"permissions": permissions}, &keyResp)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("issue api key: expected 201, got %d", resp.StatusCode)
		}
		return keyResp.APIKey, keyResp.HMACSecret
	}

	sign := func(secret, method, path string) (string, string) {
		ts := jsonNumberString(time.Now().UnixMilli())
		return ts, gateway.Sign(secret, method, path, ts, nil)
	}

	// A key scoped only to sessions:read must be refused sessions:write.
	readOnlyKey, readOnlySecret := issueKey([]string{gateway.PermissionSessionsRead})
	ts, sig := sign(readOnlySecret, http.MethodPost, "/v2/sessions")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/sessions", nil)
	req.Header.Set("X-API-Key", readOnlyKey)
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", sig)
	writeResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v2/sessions with read-only key: %v", err)
	}
	defer writeResp.Body.Close()
	if writeResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 creating a session with a sessions:read-only key, got %d", writeResp.StatusCode)
	}

	// A key scoped only to sessions:write must be refused sessions:read.
	writeOnlyKey, writeOnlySecret := issueKey([]string{gateway.PermissionSessionsWrite})
	path := "/v2/sessions/" + uuid.NewString()
	ts, sig = sign(writeOnlySecret, http.MethodGet, path)
	req, _ = http.NewRequest(http.MethodGet, srv.URL+path, nil)
	req.Header.Set("X-API-Key", writeOnlyKey)
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", sig)
	readResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s with write-only key: %v", path, err)
	}
	defer readResp.Body.Close()
	if readResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 reading a session with a sessions:write-only key, got %d", readResp.StatusCode)
	}
}
