// Phase 5 acceptance test: drives session creation, the deposit event
// pipeline, and compliance_hold resolution through the real HTTP router
// (both the tenant gateway surface and the admin surface), proving
// internal/session actually composes with corridor/compliance/rate/
// treasury/tenant/eventbus end to end — not just that each passes its own
// package tests in isolation (see onboarding_test.go for the same style
// applied to Phase 2).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

func uniqueEVMAddress(t *testing.T) string {
	t.Helper()
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random address: %v", err)
	}
	return "0x" + hex.EncodeToString(b)
}

func TestSessionCreateAndDepositFlowOverHTTP(t *testing.T) {
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

	// --- Fixture setup (below the onboarding HTTP surface — corridor
	// config, system rates, and collection-provider bindings are ops
	// actions with no dedicated onboarding endpoint yet, same scoping note
	// onboarding_test.go already makes for corridor.UpsertCorridor). ---
	tenantID, err := stores.tenant.CreateTenant(ctx, "Session HTTP Test "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := stores.tenant.ApproveKYB(ctx, tenantID); err != nil {
		t.Fatalf("approve kyb: %v", err)
	}
	apiKey, hmacSecret, err := stores.tenant.IssueAPIKey(ctx, tenantID)
	if err != nil {
		t.Fatalf("issue api key: %v", err)
	}

	fiatCurrency := "TST" + uuid.NewString()[:8]
	corridorID, err := stores.corridor.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:           "USDT",
		CryptoNetwork:         string(wallet.Ethereum),
		FiatCurrency:          fiatCurrency,
		Active:                true,
		TravelRuleWindow:      time.Hour,
		ComplianceHoldTimeout: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}
	if err := stores.tenant.SetCorridorEntitlement(ctx, tenantID, corridorID, true, nil); err != nil {
		t.Fatalf("set corridor entitlement: %v", err)
	}
	if err := stores.rate.SetSystemRate(ctx, fiatCurrency, decimal.NewFromInt(1000), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}
	// LockRate (via session.CreateSession) reads the persisted current_rates
	// snapshot, not GetBestQuote live — seed it the same way
	// rate.CurrentRateJob would.
	if _, err := stores.rate.ComputeAndPersistCurrentRate(ctx, fiatCurrency); err != nil {
		t.Fatalf("compute and persist current rate: %v", err)
	}
	if err := stores.treasury.RegisterTenantCustomWallet(ctx, tenantID, wallet.Ethereum, uniqueEVMAddress(t), ""); err != nil {
		t.Fatalf("register tenant custom wallet: %v", err)
	}
	if _, err := stores.corridor.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeCollection, "tenant_provided_wallet", 1, true, nil); err != nil {
		t.Fatalf("upsert collection provider binding: %v", err)
	}

	sign := func(method, path string, body []byte) (string, string) {
		ts := jsonNumberString(time.Now().UnixMilli())
		return ts, gateway.Sign(hmacSecret, method, path, ts, body)
	}
	doSigned := func(method, path string, body []byte, out any) *http.Response {
		ts, sig := sign(method, path, body)
		var bodyReader *bytes.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		} else {
			bodyReader = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, srv.URL+path, bodyReader)
		req.Header.Set("X-API-Key", apiKey)
		req.Header.Set("X-Timestamp", ts)
		req.Header.Set("X-Signature", sig)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("decode %s %s response: %v", method, path, err)
			}
		}
		return resp
	}

	// No screening vendor is configured by default (project decision — see
	// ARCHITECTURE.md §5) so this real, unmodified deployment path lands in
	// compliance_hold, exactly like the KYB flow onboarding_test.go
	// exercises for the same reason.
	createBody, _ := json.Marshal(map[string]string{
		"crypto_asset": "USDT", "crypto_network": string(wallet.Ethereum),
		"fiat_currency": fiatCurrency, "fiat_amount": "100.00",
	})
	var createResp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	resp := doSigned(http.MethodPost, "/v2/sessions", createBody, &createResp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: expected 201, got %d", resp.StatusCode)
	}
	if createResp.Status != "compliance_hold" {
		t.Fatalf("expected compliance_hold with no screening vendor configured, got %s", createResp.Status)
	}
	sessionID := createResp.ID

	// GET must round-trip the same status, and must never leak an internal
	// compliance_case_id field (ARCHITECTURE.md §5).
	var getResp map[string]json.RawMessage
	resp = doSigned(http.MethodGet, "/v2/sessions/"+sessionID, nil, &getResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get session: expected 200, got %d", resp.StatusCode)
	}
	if _, leaked := getResp["compliance_case_id"]; leaked {
		t.Fatal("expected compliance_case_id to never appear in the tenant-facing response")
	}

	// A different tenant's credential must not be able to read this
	// session — 404, not 403, so existence isn't confirmed either way.
	otherTenantID, err := stores.tenant.CreateTenant(ctx, "Other Tenant "+uuid.NewString())
	if err != nil {
		t.Fatalf("create other tenant: %v", err)
	}
	if err := stores.tenant.ApproveKYB(ctx, otherTenantID); err != nil {
		t.Fatalf("approve other tenant kyb: %v", err)
	}
	otherAPIKey, otherHMACSecret, err := stores.tenant.IssueAPIKey(ctx, otherTenantID)
	if err != nil {
		t.Fatalf("issue other tenant api key: %v", err)
	}
	otherTS := jsonNumberString(time.Now().UnixMilli())
	otherSig := gateway.Sign(otherHMACSecret, http.MethodGet, "/v2/sessions/"+sessionID, otherTS, nil)
	otherReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/sessions/"+sessionID, nil)
	otherReq.Header.Set("X-API-Key", otherAPIKey)
	otherReq.Header.Set("X-Timestamp", otherTS)
	otherReq.Header.Set("X-Signature", otherSig)
	otherResp, err := client.Do(otherReq)
	if err != nil {
		t.Fatalf("other tenant get session: %v", err)
	}
	defer otherResp.Body.Close()
	if otherResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's session, got %d", otherResp.StatusCode)
	}

	// --- Admin resolves the hold, driving compliance_hold -> pending. ---
	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	adminEmail := "session-test-" + uuid.NewString() + "@sirfi.test"
	if _, err := adminStore.CreateAdmin(ctx, adminEmail, "correct-horse-battery-staple"); err != nil {
		t.Fatalf("provision admin: %v", err)
	}
	var loginResp struct {
		Token string `json:"token"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/login", "", map[string]string{
		"email": adminEmail, "password": "correct-horse-battery-staple",
	}, &loginResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login: expected 200, got %d", resp.StatusCode)
	}

	var resolveResp struct {
		Status string `json:"status"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/sessions/"+sessionID+"/resolve", loginResp.Token, map[string]any{
		"approved": true, "reason": "manually verified",
	}, &resolveResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve session hold: expected 200, got %d", resp.StatusCode)
	}
	if resolveResp.Status != "pending" {
		t.Fatalf("expected pending after hold resolution, got %s", resolveResp.Status)
	}

	// --- Drive the deposit event pipeline: pending -> deposit_detected ->
	// deposit_confirmed, via the real eventbus dispatcher. ---
	var pendingResp struct {
		DepositReservationID string `json:"deposit_reservation_id"`
	}
	resp = doSigned(http.MethodGet, "/v2/sessions/"+sessionID, nil, &pendingResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get pending session: expected 200, got %d", resp.StatusCode)
	}
	reservationID, err := uuid.Parse(pendingResp.DepositReservationID)
	if err != nil {
		t.Fatalf("parse deposit_reservation_id: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go stores.bus.Run(runCtx, 30*time.Millisecond)

	publish := func(eventType string) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		payload, _ := json.Marshal(map[string]string{"tx_reference": "http-test-" + uuid.NewString()})
		if err := stores.bus.Publish(ctx, tx, eventbus.Event{
			EventType: eventType, AggregateType: "treasury_deposit", AggregateID: reservationID, Payload: payload,
		}); err != nil {
			t.Fatalf("publish %s: %v", eventType, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	publish("treasury.deposit_detected")
	waitForHTTPStatus(t, doSigned, sessionID, "deposit_detected")

	publish("treasury.deposit_confirmed")
	waitForHTTPStatus(t, doSigned, sessionID, "deposit_confirmed")
}

func waitForHTTPStatus(t *testing.T, doSigned func(method, path string, body []byte, out any) *http.Response, sessionID, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var resp struct {
			Status string `json:"status"`
		}
		doSigned(http.MethodGet, "/v2/sessions/"+sessionID, nil, &resp)
		if resp.Status == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("session %s did not reach status %s within timeout", sessionID, want)
}

