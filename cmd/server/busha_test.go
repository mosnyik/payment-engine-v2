package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
	"github.com/sirfi/payment-engine-v2/internal/treasury"
)

func TestBushaSandboxCollectionOverHTTP(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fiat := "BUSH" + uuid.NewString()[:8]
	ref, address := "TRF_"+uuid.NewString(), "busha-test-"+uuid.NewString()
	expires := time.Now().Add(10 * time.Minute).UTC()
	const amount = "0.101011" // 100 fiat / (1000 * 0.99), rounded up to six places
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		if r.URL.Path == "/v1/quotes" {
			if in["source_amount"] != amount {
				t.Errorf("unexpected amount: %v", in["source_amount"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]string{"id": "QUO_test"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"id": ref, "source_currency": "USDT", "target_currency": "USDT", "source_amount": amount, "status": "pending",
			"pay_in": map[string]any{"type": "address", "network": "TRX", "address": address, "expires_at": expires},
		}})
	}))
	defer provider.Close()
	cfg.SandboxMode, cfg.SandboxCollectionProvider, cfg.SandboxCorridors = true, "busha", "USDT:TRX:"+fiat
	cfg.Treasury.Busha = config.CollectionProviderConfig{Enabled: true, APIURL: provider.URL, APIKey: "test-key", WebhookSecret: "test-signing-secret"}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	stores, err := buildStores(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	corr, err := stores.corridor.GetCorridor(ctx, "USDT", "TRX", fiat)
	if err != nil {
		t.Fatal(err)
	}
	// Re-seeding must remove a stale fake collection binding.
	if _, err := stores.corridor.UpsertProviderBinding(ctx, corr.ID, corridor.ProviderTypeCollection, "sandbox", 0, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := seedSandboxCorridors(ctx, stores.corridor, stores.compliance, cfg.SandboxCorridors, "busha"); err != nil {
		t.Fatal(err)
	}
	bindings, err := stores.corridor.ListActiveProviders(ctx, corr.ID, corridor.ProviderTypeCollection)
	if err != nil || len(bindings) != 1 || bindings[0].ProviderName != "busha" {
		t.Fatalf("unexpected bindings: %v %v", bindings, err)
	}
	tenantID, err := stores.tenant.CreateTenant(ctx, "Busha Test "+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.tenant.ApproveKYB(ctx, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := stores.tenant.SetCorridorEntitlement(ctx, tenantID, corr.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	key, secret, err := stores.tenant.IssueAPIKey(ctx, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.rate.SetSystemRate(ctx, fiat, decimal.NewFromInt(1000), decimal.Zero, decimal.Zero); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.rate.ComputeAndPersistCurrentRate(ctx, fiat); err != nil {
		t.Fatal(err)
	}
	router, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, body []byte, webhook bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if webhook {
			r.Header.Set("X-BU-Signature", treasury.ComputeBushaSignature("test-signing-secret", body))
		} else {
			ts := jsonNumberString(time.Now().UnixMilli())
			r.Header.Set("X-API-Key", key)
			r.Header.Set("X-Timestamp", ts)
			r.Header.Set("X-Signature", gateway.Sign(secret, method, path, ts, body))
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	body, _ := json.Marshal(map[string]string{"crypto_asset": "USDT", "crypto_network": "TRX", "fiat_currency": fiat, "fiat_amount": "100"})
	w := do("POST", "/v2/sessions", body, false)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created sessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	d := created.DepositInstructions
	if d == nil || d.Address != address || d.Amount != amount || !d.ExpiresAt.Equal(expires) {
		t.Fatalf("missing provider instructions: %+v", d)
	}
	w = do("GET", "/v2/sessions/"+created.ID.String(), nil, false)
	var fetched sessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || fetched.DepositInstructions == nil || fetched.DepositInstructions.Address != address {
		t.Fatal("instructions not persisted")
	}
	go stores.bus.Run(ctx, 10*time.Millisecond)
	// A confirmation without processing, followed by another success event,
	// must credit the transfer exactly once and create only one settlement.
	for _, status := range []string{"funds_received", "completed", "completed"} {
		body, _ := json.Marshal(map[string]any{"event": "transfer." + status, "data": map[string]string{
			"id": ref, "status": status, "source_currency": "USDT", "target_currency": "USDT", "source_amount": amount,
		}})
		w = do("POST", "/v2/webhooks/treasury/busha", body, true)
		if w.Code != 200 {
			t.Fatalf("webhook: %d %s", w.Code, w.Body)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM settlements WHERE session_id=$1`, created.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			var deposits int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM treasury_deposits WHERE reservation_id=$1 AND status='confirmed'`, d.ReservationID).Scan(&deposits); err != nil {
				t.Fatal(err)
			}
			if deposits != 1 {
				t.Fatalf("expected one deposit, got %d", deposits)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("confirmed Busha transfer did not reach settlement")
}
