// POST /v2/admin/corridors through the real router: create with bindings,
// create-only (409), validation, provider-name checks leaving nothing
// behind, and the admin audit-log entry.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func TestAdminCreateCorridor(t *testing.T) {
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

	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	adminEmail := "corridor-admin-" + uuid.NewString() + "@sirfi.test"
	adminID, err := adminStore.CreateAdmin(ctx, adminEmail, "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("provision admin: %v", err)
	}
	var login struct {
		Token string `json:"token"`
	}
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/login", "", map[string]string{
		"email": adminEmail, "password": "correct-horse-battery-staple",
	}, &login)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login: expected 200, got %d", resp.StatusCode)
	}

	create := func(body map[string]any, out any) *http.Response {
		return doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/corridors", login.Token, body, out)
	}
	fiat := "TST" + strings.ToUpper(uuid.NewString()[:8])

	// --- An unknown provider name is rejected, and nothing is left behind ---
	resp = create(map[string]any{
		"crypto_asset": "usdt", "crypto_network": "trc20", "fiat_currency": fiat,
		"providers": []map[string]any{{"type": "settlement", "name": "no-such-bank"}},
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown provider: expected 400, got %d", resp.StatusCode)
	}

	// --- Happy path: lowercase input normalized, defaults applied ---
	var created struct {
		ID                        string   `json:"id"`
		CryptoAsset               string   `json:"crypto_asset"`
		CryptoNetwork             string   `json:"crypto_network"`
		FiatCurrency              string   `json:"fiat_currency"`
		Active                    bool     `json:"active"`
		MinAmountFiat             *string  `json:"min_amount_fiat"`
		MaxAmountFiat             *string  `json:"max_amount_fiat"`
		TravelRuleWindowSeconds   int64    `json:"travel_rule_window_seconds"`
		ComplianceHoldTimeoutSecs int64    `json:"compliance_hold_timeout_seconds"`
		RequiredDestinationFields []string `json:"required_destination_fields"`
		Providers                 []struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			Priority int    `json:"priority"`
		} `json:"providers"`
	}
	resp = create(map[string]any{
		"crypto_asset": "usdt", "crypto_network": "trc20", "fiat_currency": strings.ToLower(fiat),
		"min_amount_fiat": "1000", "max_amount_fiat": "5000000",
		"required_destination_fields": []string{"account_number", "bank_code"},
		"providers": []map[string]any{
			{"type": "collection", "name": "busha", "priority": 0},
			{"type": "settlement", "name": "flutterwave", "priority": 0},
			{"type": "settlement", "name": "paystack", "priority": 1},
		},
	}, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create corridor: expected 201, got %d", resp.StatusCode)
	}
	if created.CryptoAsset != "USDT" || created.CryptoNetwork != "TRC20" || created.FiatCurrency != fiat || !created.Active {
		t.Fatalf("expected normalized, active corridor, got %+v", created)
	}
	if created.MinAmountFiat == nil || *created.MinAmountFiat != "1000" || created.MaxAmountFiat == nil || *created.MaxAmountFiat != "5000000" {
		t.Fatalf("expected limits 1000..5000000, got %v..%v", created.MinAmountFiat, created.MaxAmountFiat)
	}
	if created.TravelRuleWindowSeconds != 86400 || created.ComplianceHoldTimeoutSecs != 86400 {
		t.Fatalf("expected 24h default windows, got %d/%d", created.TravelRuleWindowSeconds, created.ComplianceHoldTimeoutSecs)
	}
	if len(created.Providers) != 3 {
		t.Fatalf("expected 3 provider bindings, got %+v", created.Providers)
	}
	corridorID := uuid.MustParse(created.ID)

	// Bindings are really persisted, in priority order.
	settlementBindings, err := stores.corridor.ListActiveProviders(ctx, corridorID, corridor.ProviderTypeSettlement)
	if err != nil {
		t.Fatalf("list settlement bindings: %v", err)
	}
	if len(settlementBindings) != 2 || settlementBindings[0].ProviderName != "flutterwave" || settlementBindings[1].ProviderName != "paystack" {
		t.Fatalf("expected settlement bindings flutterwave, paystack, got %+v", settlementBindings)
	}

	// Visible on the existing admin read endpoint.
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/corridors/"+created.ID, login.Token, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get created corridor: expected 200, got %d", resp.StatusCode)
	}

	// --- Create-only: the same asset/network/fiat is a 409, not an overwrite ---
	resp = create(map[string]any{
		"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": fiat, "min_amount_fiat": "1",
	}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate corridor: expected 409, got %d", resp.StatusCode)
	}
	c, err := stores.corridor.GetCorridorByID(ctx, corridorID)
	if err != nil {
		t.Fatalf("get corridor: %v", err)
	}
	if c.MinAmountFiat == nil || c.MinAmountFiat.String() != "1000" {
		t.Fatalf("expected the 409 to leave min_amount_fiat at 1000, got %v", c.MinAmountFiat)
	}

	// --- Audit trail ---
	entries, _, err := adminStore.ListAuditLog(ctx, &adminID, 10, 0)
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "corridor.create" && e.Target != nil && *e.Target == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a corridor.create audit entry for %s, got %+v", created.ID, entries)
	}

	// --- Validation ---
	other := "TST" + strings.ToUpper(uuid.NewString()[:8])
	for name, body := range map[string]map[string]any{
		"missing fiat":       {"crypto_asset": "USDT", "crypto_network": "TRC20"},
		"min above max":      {"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": other, "min_amount_fiat": "10", "max_amount_fiat": "5"},
		"bad decimal":        {"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": other, "max_amount_fiat": "lots"},
		"zero amount":        {"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": other, "travel_rule_threshold_fiat": "0"},
		"zero window":        {"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": other, "compliance_hold_timeout_seconds": 0},
		"bad provider type":  {"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": other, "providers": []map[string]any{{"type": "rate", "name": "busha"}}},
		"duplicate provider": {"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": other, "providers": []map[string]any{{"type": "collection", "name": "busha"}, {"type": "collection", "name": "busha"}}},
		"negative priority":  {"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": other, "providers": []map[string]any{{"type": "collection", "name": "busha", "priority": -1}}},
	} {
		if resp := create(body, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, resp.StatusCode)
		}
	}

	// --- Admin-only ---
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/corridors", "", map[string]any{
		"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": other,
	}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create: expected 401, got %d", resp.StatusCode)
	}
}
