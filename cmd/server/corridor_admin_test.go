// Admin corridor management through the real router: POST /v2/admin/corridors
// (create with bindings, create-only 409, validation, provider-name checks,
// audit entry), PATCH /v2/admin/corridors/{id} (partial edit, null clears,
// merged validation, deactivate/reactivate) and PUT .../providers (atomic
// binding replacement).
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

type corridorAdminTestEnv struct {
	srv        *httptest.Server
	client     *http.Client
	stores     *appStores
	adminStore *adminauth.Store
	adminID    uuid.UUID
	token      string
}

func newCorridorAdminTestEnv(t *testing.T) *corridorAdminTestEnv {
	t.Helper()
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
	return &corridorAdminTestEnv{srv: srv, client: client, stores: stores, adminStore: adminStore, adminID: adminID, token: login.Token}
}

// hasAuditEntry reports whether the env's admin has an audit-log entry with
// this action and target.
func (e *corridorAdminTestEnv) hasAuditEntry(t *testing.T, action, target string) bool {
	t.Helper()
	entries, _, err := e.adminStore.ListAuditLog(context.Background(), &e.adminID, 20, 0)
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	for _, en := range entries {
		if en.Action == action && en.Target != nil && *en.Target == target {
			return true
		}
	}
	return false
}

func TestAdminCreateCorridor(t *testing.T) {
	env := newCorridorAdminTestEnv(t)
	ctx := context.Background()
	srv, client, stores := env.srv, env.client, env.stores
	token := env.token

	create := func(body map[string]any, out any) *http.Response {
		return doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/corridors", token, body, out)
	}
	fiat := "TST" + strings.ToUpper(uuid.NewString()[:8])

	// --- An unknown provider name is rejected, and nothing is left behind ---
	resp := create(map[string]any{
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
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/corridors/"+created.ID, token, nil, nil)
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
	if !env.hasAuditEntry(t, "corridor.create", created.ID) {
		t.Fatalf("expected a corridor.create audit entry for %s", created.ID)
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

func TestAdminUpdateAndDeactivateCorridor(t *testing.T) {
	env := newCorridorAdminTestEnv(t)
	ctx := context.Background()
	base := env.srv.URL + "/v2/admin/corridors/"

	fiat := "TST" + strings.ToUpper(uuid.NewString()[:8])
	var created struct {
		ID string `json:"id"`
	}
	resp := doJSON(t, env.client, http.MethodPost, env.srv.URL+"/v2/admin/corridors", env.token, map[string]any{
		"crypto_asset": "USDT", "crypto_network": "TRC20", "fiat_currency": fiat,
		"min_amount_fiat": "1000", "max_amount_fiat": "5000000", "travel_rule_threshold_fiat": "1500000",
		"providers": []map[string]any{{"type": "settlement", "name": "flutterwave"}},
	}, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create corridor: expected 201, got %d", resp.StatusCode)
	}
	id := uuid.MustParse(created.ID)

	type corridorBody struct {
		Active                  bool     `json:"active"`
		MinAmountFiat           *string  `json:"min_amount_fiat"`
		MaxAmountFiat           *string  `json:"max_amount_fiat"`
		TravelRuleThresholdFiat *string  `json:"travel_rule_threshold_fiat"`
		TravelRuleWindowSeconds int64    `json:"travel_rule_window_seconds"`
		RequiredFields          []string `json:"required_destination_fields"`
	}
	patch := func(body map[string]any, out any) *http.Response {
		return doJSON(t, env.client, http.MethodPatch, base+created.ID, env.token, body, out)
	}

	// --- Partial update: only the sent fields change ---
	var got corridorBody
	resp = patch(map[string]any{"max_amount_fiat": "9000000", "travel_rule_window_seconds": 3600, "required_destination_fields": []string{"account_number"}}, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("partial update: expected 200, got %d", resp.StatusCode)
	}
	if got.MaxAmountFiat == nil || *got.MaxAmountFiat != "9000000" || got.TravelRuleWindowSeconds != 3600 || len(got.RequiredFields) != 1 {
		t.Fatalf("expected max 9000000, window 3600, 1 required field, got %+v", got)
	}
	if got.MinAmountFiat == nil || *got.MinAmountFiat != "1000" || !got.Active {
		t.Fatalf("expected untouched min 1000 and still active, got %+v", got)
	}

	// --- Explicit null clears a nullable field ---
	got = corridorBody{}
	resp = patch(map[string]any{"travel_rule_threshold_fiat": nil}, &got)
	if resp.StatusCode != http.StatusOK || got.TravelRuleThresholdFiat != nil {
		t.Fatalf("clear threshold: expected 200 and no threshold, got %d %+v", resp.StatusCode, got)
	}

	// --- Validation runs on the merged result, and a rejected patch writes nothing ---
	if resp = patch(map[string]any{"max_amount_fiat": "500"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("max below existing min: expected 400, got %d", resp.StatusCode)
	}
	for name, body := range map[string]map[string]any{
		"natural key":   {"fiat_currency": "EUR"},
		"unknown field": {"max_amount": "10"},
		"empty body":    {},
		"bad window":    {"compliance_hold_timeout_seconds": 0},
		"null active":   {"active": nil},
	} {
		if resp := patch(body, nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, resp.StatusCode)
		}
	}
	c, err := env.stores.corridor.GetCorridorByID(ctx, id)
	if err != nil {
		t.Fatalf("get corridor: %v", err)
	}
	if c.MaxAmountFiat == nil || c.MaxAmountFiat.String() != "9000000" || c.FiatCurrency != fiat {
		t.Fatalf("expected rejected patches to leave the corridor unchanged, got %+v", c)
	}
	if !env.hasAuditEntry(t, "corridor.update", created.ID) {
		t.Fatal("expected a corridor.update audit entry")
	}

	if resp = doJSON(t, env.client, http.MethodPatch, base+uuid.NewString(), env.token, map[string]any{"active": false}, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown corridor: expected 404, got %d", resp.StatusCode)
	}

	// --- Deactivate: new sessions no longer find it, entitlements survive ---
	tenantID, err := env.stores.tenant.RegisterTenant(ctx, "Corridor Edit "+uuid.NewString(), "owner+"+uuid.NewString()+"@example.com")
	if err != nil {
		t.Fatalf("register tenant: %v", err)
	}
	if err := env.stores.tenant.SetCorridorEntitlement(ctx, tenantID, id, true, nil); err != nil {
		t.Fatalf("set entitlement: %v", err)
	}
	if resp = patch(map[string]any{"active": false}, &got); resp.StatusCode != http.StatusOK || got.Active {
		t.Fatalf("deactivate: expected 200 and inactive, got %d %+v", resp.StatusCode, got)
	}
	// GetCorridor is the lookup session.CreateSession uses.
	if _, err := env.stores.corridor.GetCorridor(ctx, "USDT", "TRC20", fiat); err != corridor.ErrNotFound {
		t.Fatalf("expected a deactivated corridor to be unavailable for new sessions, got %v", err)
	}

	// --- Reactivate: available again, entitlement intact without a re-grant ---
	if resp = patch(map[string]any{"active": true}, &got); resp.StatusCode != http.StatusOK || !got.Active {
		t.Fatalf("reactivate: expected 200 and active, got %d %+v", resp.StatusCode, got)
	}
	if _, err := env.stores.corridor.GetCorridor(ctx, "USDT", "TRC20", fiat); err != nil {
		t.Fatalf("expected a reactivated corridor to be available, got %v", err)
	}
	if ok, err := env.stores.tenant.CheckEntitlement(ctx, tenantID, id); err != nil || !ok {
		t.Fatalf("expected the entitlement to survive deactivation, got %v (err %v)", ok, err)
	}

	// --- PUT providers replaces the whole set atomically ---
	put := func(body any) *http.Response {
		return doJSON(t, env.client, http.MethodPut, base+created.ID+"/providers", env.token, body, nil)
	}
	if resp = put(map[string]any{"providers": []map[string]any{
		{"type": "settlement", "name": "paystack", "priority": 0},
		{"type": "settlement", "name": "monnify", "priority": 1},
		{"type": "collection", "name": "busha"},
	}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("replace providers: expected 200, got %d", resp.StatusCode)
	}
	settlementBindings, err := env.stores.corridor.ListActiveProviders(ctx, id, corridor.ProviderTypeSettlement)
	if err != nil {
		t.Fatalf("list settlement bindings: %v", err)
	}
	if len(settlementBindings) != 2 || settlementBindings[0].ProviderName != "paystack" || settlementBindings[1].ProviderName != "monnify" {
		t.Fatalf("expected flutterwave replaced by paystack, monnify, got %+v", settlementBindings)
	}
	if !env.hasAuditEntry(t, "corridor.providers.replace", created.ID) {
		t.Fatal("expected a corridor.providers.replace audit entry")
	}

	// A bad entry rejects the whole replacement, so the previous set stays.
	if resp = put(map[string]any{"providers": []map[string]any{{"type": "settlement", "name": "no-such-bank"}}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown provider: expected 400, got %d", resp.StatusCode)
	}
	if resp = put(map[string]any{}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing providers key: expected 400, got %d", resp.StatusCode)
	}
	if after, _ := env.stores.corridor.ListActiveProviders(ctx, id, corridor.ProviderTypeSettlement); len(after) != 2 {
		t.Fatalf("expected rejected replacements to leave 2 settlement bindings, got %+v", after)
	}

	// Re-adding a previously removed binding reactivates it; [] removes all.
	if resp = put(map[string]any{"providers": []map[string]any{{"type": "settlement", "name": "flutterwave"}}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("re-add flutterwave: expected 200, got %d", resp.StatusCode)
	}
	if b, _ := env.stores.corridor.ListActiveProviders(ctx, id, corridor.ProviderTypeSettlement); len(b) != 1 || b[0].ProviderName != "flutterwave" {
		t.Fatalf("expected only flutterwave active, got %+v", b)
	}
	if resp = put(map[string]any{"providers": []map[string]any{}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("remove all: expected 200, got %d", resp.StatusCode)
	}
	if b, _ := env.stores.corridor.ListActiveProviders(ctx, id, corridor.ProviderTypeCollection); len(b) != 0 {
		t.Fatalf("expected no active collection bindings after [], got %+v", b)
	}

	if resp = doJSON(t, env.client, http.MethodPut, base+uuid.NewString()+"/providers", env.token, map[string]any{"providers": []map[string]any{}}, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown corridor providers: expected 404, got %d", resp.StatusCode)
	}

	// --- Admin-only ---
	if resp = doJSON(t, env.client, http.MethodPatch, base+created.ID, "", map[string]any{"active": false}, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated patch: expected 401, got %d", resp.StatusCode)
	}
}
