// Phase 6 acceptance test: drives session creation (including the payout-
// destination validation added in ARCHITECTURE.md §8's "Payout destination"
// decision) through deposit confirmation and into the real DispatchWorker,
// then the ops-facing admin surface — over the actual HTTP router, not just
// internal/settlement's own Store-level tests. Also proves both inbound
// webhook routes (/webhooks/settlement/{providerName} and the newly-mounted
// /webhooks/treasury/busha) are actually reachable and signature-gated, not
// just implemented and never wired up.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
	"github.com/sirfi/payment-engine-v2/internal/settlement"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

func TestSettlementDispatchAndOpsSurfaceOverHTTP(t *testing.T) {
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

	// --- Fixture setup, same scoping note as session_test.go: corridor
	// config/system rate/collection binding are ops actions with no
	// dedicated onboarding endpoint yet. RequiredDestinationFields exercises
	// the corridor-owns-the-requirement design over real HTTP, not just
	// internal/corridor's own unit test. No settlement provider binding is
	// registered — testConfig's zero-value settlement.Config means none of
	// the five named adapters are enabled, so DispatchWorker deterministically
	// reaches the real "no active, enabled settlement provider" terminal
	// path (dispatch.go) rather than needing a fake provider this package
	// has no way to inject. ---
	tenantID, err := stores.tenant.CreateTenant(ctx, "Settlement HTTP Test "+uuid.NewString())
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
		CryptoAsset:               "USDT",
		CryptoNetwork:             string(wallet.Ethereum),
		FiatCurrency:              fiatCurrency,
		Active:                    true,
		TravelRuleWindow:          time.Hour,
		ComplianceHoldTimeout:     24 * time.Hour,
		RequiredDestinationFields: []string{"account_number", "bank_code"},
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
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(string(body)))
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

	// --- An incomplete payout_destination must fail fast at CreateSession,
	// before screening/rate-lock/address-reservation ever run (ARCHITECTURE.md
	// §8 "Payout destination"). ---
	incompleteBody, _ := json.Marshal(map[string]any{
		"crypto_asset": "USDT", "crypto_network": string(wallet.Ethereum),
		"fiat_currency": fiatCurrency, "fiat_amount": "100.00",
		"payout_destination": map[string]string{"account_number": "0123456789"}, // bank_code missing
	})
	var errResp struct {
		Error string `json:"error"`
	}
	resp := doSigned(http.MethodPost, "/v2/sessions", incompleteBody, &errResp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create session with incomplete destination: expected 400, got %d", resp.StatusCode)
	}
	if !strings.Contains(errResp.Error, "bank_code") {
		t.Fatalf("expected error to name the missing field bank_code, got %q", errResp.Error)
	}

	// --- A valid destination proceeds exactly like session_test.go's flow:
	// no screening vendor configured -> compliance_hold. ---
	createBody, _ := json.Marshal(map[string]any{
		"crypto_asset": "USDT", "crypto_network": string(wallet.Ethereum),
		"fiat_currency": fiatCurrency, "fiat_amount": "100.00",
		"payout_destination": map[string]string{"account_number": "0123456789", "bank_code": "044"},
	})
	var createResp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	resp = doSigned(http.MethodPost, "/v2/sessions", createBody, &createResp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: expected 201, got %d", resp.StatusCode)
	}
	if createResp.Status != "compliance_hold" {
		t.Fatalf("expected compliance_hold with no screening vendor configured, got %s", createResp.Status)
	}
	sessionID := createResp.ID

	// --- Admin resolves the hold, driving compliance_hold -> pending. ---
	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	adminEmail := "settlement-test-" + uuid.NewString() + "@sirfi.test"
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

	// DispatchWorker computes the settlement amount from the actual confirmed
	// deposit (treasury.GetConfirmedDepositTotal), not from the session's
	// requested fiat_amount — a real row is required here, same as
	// internal/settlement/settlement_test.go's createDepositConfirmedSession.
	if _, err := pool.Exec(ctx,
		`INSERT INTO treasury_deposits (reservation_id, status, crypto_asset, amount, tx_reference, confirmed_at)
		 VALUES ($1, 'confirmed', 'USDT', $2, $3, now())`,
		reservationID, decimal.NewFromInt(100), "settlement-http-test-deposit-"+uuid.NewString(),
	); err != nil {
		t.Fatalf("insert confirmed deposit: %v", err)
	}

	// --- Background jobs are wired in main.go, not buildStores/buildRouter
	// (see main.go's run()) — start them explicitly here, same convention
	// session_test.go already establishes for the bus. ---
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go stores.bus.Run(runCtx, 30*time.Millisecond)
	dispatchWorker := settlement.NewDispatchWorker(stores.settlement, 30*time.Millisecond)
	go dispatchWorker.Run(runCtx)

	publish := func(eventType string) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		payload, _ := json.Marshal(map[string]string{"tx_reference": "settlement-http-test-" + uuid.NewString()})
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

	// DispatchWorker picks the settlement up, posts the deferred ledger
	// transactions, and — with no settlement provider enabled in this test's
	// config — reaches settlement_failed via the real dispatch.go path
	// (ARCHITECTURE.md §8's ops-paged terminal state), exactly as it would
	// in production before a real settlement partner is configured.
	waitForHTTPStatus(t, doSigned, sessionID, "settlement_failed")

	// --- Ops surface: the failed settlement must be visible and correctly
	// attributed over the real admin HTTP route. ---
	var listResp []struct {
		ID        string `json:"id"`
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
		OpsPaged  bool   `json:"ops_paged"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/settlements?status=settlement_failed", loginResp.Token, nil, &listResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list settlements: expected 200, got %d", resp.StatusCode)
	}
	found := false
	for _, s := range listResp {
		if s.SessionID == sessionID {
			found = true
			if !s.OpsPaged {
				t.Fatal("expected ops_paged true on the failed settlement")
			}
		}
	}
	if !found {
		t.Fatalf("expected session %s's settlement in the settlement_failed ops list, got %+v", sessionID, listResp)
	}

	// --- Both inbound webhook routes must be actually reachable and
	// self-verified, not just implemented in the package. A router-level
	// 404 (route never mounted) and a handler-level 404/401 (route mounted,
	// business logic ran) are distinguished by asserting on the JSON error
	// body a real handler produces. ---
	settlementWebhookResp := doJSON(t, client, http.MethodPost, srv.URL+"/v2/webhooks/settlement/nonexistent-provider", "", nil, &errResp)
	if settlementWebhookResp.StatusCode != http.StatusNotFound {
		t.Fatalf("settlement webhook, unknown provider: expected 404, got %d", settlementWebhookResp.StatusCode)
	}
	if !strings.Contains(errResp.Error, "unknown or unconfigured settlement provider") {
		t.Fatalf("expected settlement.ErrUnknownProvider's message, got %q", errResp.Error)
	}

	// treasury's webhook route (this phase's other carried-forward gap):
	// no valid Busha webhook secret is configured in testConfig, so any
	// signature is wrong — proves the route is mounted and wired to real
	// signature verification, not a 404 from an unmounted route.
	treasuryWebhookReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/webhooks/treasury/busha", strings.NewReader(`{}`))
	treasuryWebhookReq.Header.Set(treasuryWebhookSignatureHeader, "not-the-right-signature")
	treasuryWebhookResp, err := client.Do(treasuryWebhookReq)
	if err != nil {
		t.Fatalf("treasury webhook request: %v", err)
	}
	defer treasuryWebhookResp.Body.Close()
	if treasuryWebhookResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("treasury webhook, bad signature: expected 401, got %d", treasuryWebhookResp.StatusCode)
	}
	var treasuryErrResp struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(treasuryWebhookResp.Body).Decode(&treasuryErrResp); err != nil {
		t.Fatalf("decode treasury webhook response: %v", err)
	}
	if !strings.Contains(treasuryErrResp.Error, "invalid webhook signature") {
		t.Fatalf("expected treasury.ErrInvalidWebhookSignature's message, got %q", treasuryErrResp.Error)
	}
}
