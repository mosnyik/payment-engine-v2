// Tenant self-service corridor entitlement: drives GET/POST
// /v2/portal/corridors through the real router, proving the jurisdiction
// gate, the admin-revoke lock, and that a tenant can never set its own fee.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

type portalCorridorListItem struct {
	ID              string `json:"id"`
	FiatCurrency    string `json:"fiat_currency"`
	Entitled        bool   `json:"entitled"`
	DisabledByAdmin bool   `json:"disabled_by_admin"`
}

func findPortalCorridor(t *testing.T, items []portalCorridorListItem, id uuid.UUID) (portalCorridorListItem, bool) {
	t.Helper()
	for _, it := range items {
		if it.ID == id.String() {
			return it, true
		}
	}
	return portalCorridorListItem{}, false
}

func TestPortalCorridorSelfService(t *testing.T) {
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
	sender := &fakePortalEmailSender{}
	stores.tenant.SetEmailSender(sender)

	router, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	client := srv.Client()

	// Unique currencies per run, same "TST"+suffix convention rate_test.go
	// uses: approved is declared in the tenant's KYB, unapproved isn't.
	approved := "TST" + strings.ToUpper(uuid.NewString()[:8])
	unapproved := "TST" + strings.ToUpper(uuid.NewString()[:8])
	upsert := func(fiat string, active bool) uuid.UUID {
		id, err := stores.corridor.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
			CryptoAsset: "USDT", CryptoNetwork: "TRC20", FiatCurrency: fiat, Active: active,
			TravelRuleWindow: 24 * time.Hour, ComplianceHoldTimeout: 24 * time.Hour,
		})
		if err != nil {
			t.Fatalf("upsert corridor %s: %v", fiat, err)
		}
		return id
	}
	approvedID := upsert(approved, true)
	unapprovedID := upsert(unapproved, true)
	inactiveID := upsert("TST"+strings.ToUpper(uuid.NewString()[:8]), false)

	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	adminEmail := "portal-corridor-" + uuid.NewString() + "@sirfi.test"
	if _, err := adminStore.CreateAdmin(ctx, adminEmail, "correct-horse-battery-staple"); err != nil {
		t.Fatalf("provision admin: %v", err)
	}
	var adminLogin struct {
		Token string `json:"token"`
	}
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/login", "", map[string]string{
		"email": adminEmail, "password": "correct-horse-battery-staple",
	}, &adminLogin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login: expected 200, got %d", resp.StatusCode)
	}

	// --- Register + verify a tenant ---
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/register", "", map[string]string{
		"name": "Corridor Fintech " + uuid.NewString(), "email": "owner+" + uuid.NewString() + "@example.com",
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("register: expected 202, got %d", resp.StatusCode)
	}
	var verify struct {
		Token string `json:"token"`
	}
	magic := extractPortalMagicLinkToken(t, sender.lastBody)
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/verify?token="+url.QueryEscape(magic), "", nil, &verify)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify: expected 200, got %d", resp.StatusCode)
	}
	token := verify.Token
	var me struct {
		ID string `json:"id"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/me", token, nil, &me)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get me: expected 200, got %d", resp.StatusCode)
	}
	tenantID := uuid.MustParse(me.ID)

	setActive := func(id uuid.UUID, active bool) *http.Response {
		return doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/corridors/"+id.String(), token, map[string]bool{"active": active}, nil)
	}

	// Before KYB clears, enabling is refused.
	if resp = setActive(approvedID, true); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("enable before kyb approval: expected 400, got %d", resp.StatusCode)
	}

	// --- KYB declaring only the approved currency, approved by an admin ---
	var kybCase struct {
		ID string `json:"ID"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/kyb", token, map[string]any{
		"submitted_data":      map[string]string{"company": "Corridor Fintech Ltd"},
		"declared_currencies": []string{approved},
	}, &kybCase)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit kyb: expected 201, got %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/compliance/holds/"+kybCase.ID+"/resolve", adminLogin.Token, map[string]any{
		"approved": true, "reason": "documents verified",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve hold: expected 200, got %d", resp.StatusCode)
	}

	// --- List: active corridors only, none entitled yet ---
	var list []portalCorridorListItem
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/corridors", token, nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list corridors: expected 200, got %d", resp.StatusCode)
	}
	if c, ok := findPortalCorridor(t, list, approvedID); !ok || c.Entitled {
		t.Fatalf("expected approved corridor listed and not entitled, got %+v (found=%v)", c, ok)
	}
	if _, ok := findPortalCorridor(t, list, inactiveID); ok {
		t.Fatal("expected an inactive corridor to be hidden from the tenant")
	}

	// --- Enable: allowed for the KYB-covered currency only ---
	if resp = setActive(approvedID, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("enable approved corridor: expected 200, got %d", resp.StatusCode)
	}
	if resp = setActive(unapprovedID, true); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("enable corridor outside kyb: expected 400, got %d", resp.StatusCode)
	}
	if resp = setActive(inactiveID, true); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("enable inactive corridor: expected 404, got %d", resp.StatusCode)
	}
	if ok, err := stores.tenant.CheckEntitlement(ctx, tenantID, approvedID); err != nil || !ok {
		t.Fatalf("expected entitlement active after self-service enable, got %v (err %v)", ok, err)
	}

	// --- Tenant disable then re-enable: both allowed ---
	if resp = setActive(approvedID, false); resp.StatusCode != http.StatusOK {
		t.Fatalf("self disable: expected 200, got %d", resp.StatusCode)
	}
	if resp = setActive(approvedID, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("self re-enable: expected 200, got %d", resp.StatusCode)
	}

	// --- An admin-set fee override survives a tenant re-enable ---
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/corridors/"+approvedID.String(), adminLogin.Token, map[string]any{
		"active": true, "fee_bps_override": 42,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin grant with fee override: expected 200, got %d", resp.StatusCode)
	}
	setActive(approvedID, false)
	if resp = setActive(approvedID, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("re-enable after admin fee override: expected 200, got %d", resp.StatusCode)
	}
	if fee, err := stores.tenant.EffectiveFeeBps(ctx, tenantID, approvedID); err != nil || fee != 42 {
		t.Fatalf("expected admin fee override 42 to survive a tenant re-enable, got %d (err %v)", fee, err)
	}

	// --- Admin revoke sticks: the tenant can't undo it ---
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/corridors/"+approvedID.String(), adminLogin.Token, map[string]any{
		"active": false,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin revoke: expected 200, got %d", resp.StatusCode)
	}
	if resp = setActive(approvedID, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("re-enable after admin revoke: expected 403, got %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/corridors", token, nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list corridors after revoke: expected 200, got %d", resp.StatusCode)
	}
	if c, _ := findPortalCorridor(t, list, approvedID); c.Entitled || !c.DisabledByAdmin {
		t.Fatalf("expected revoked corridor listed as not entitled and disabled_by_admin, got %+v", c)
	}

	// An admin re-grant lifts the lock.
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants/"+tenantID.String()+"/corridors/"+approvedID.String(), adminLogin.Token, map[string]any{
		"active": true,
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin re-grant: expected 200, got %d", resp.StatusCode)
	}
	setActive(approvedID, false)
	if resp = setActive(approvedID, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("self re-enable after admin re-grant: expected 200, got %d", resp.StatusCode)
	}

	// --- Request validation ---
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/corridors/"+approvedID.String(), token, map[string]any{}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing active field: expected 400, got %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/corridors/not-a-uuid", token, map[string]bool{"active": true}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad corridor id: expected 400, got %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/corridors", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: expected 401, got %d", resp.StatusCode)
	}
}
