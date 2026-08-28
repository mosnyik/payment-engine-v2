// Phase 11 acceptance test: drives the admin read/browse surface
// (docs/IMPLEMENTATION_PLAN.md's Phase 11 section) through real HTTP
// requests against the actual router — list/detail/pagination/filtering for
// tenants, corridors, per-tenant sessions, and both audit logs. Reuses
// testConfig/doJSON from onboarding_test.go (same package).
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func TestAdminBrowseSurface(t *testing.T) {
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

	// request_audit_log rows are written asynchronously by a background
	// drain of the buffered channel gateway.NewRouter's audit middleware
	// writes to (internal/platform/audit.Logger.Run) — started here the same
	// way cmd/server/main.go starts it, since buildRouter alone never does.
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go stores.audit.Run(runCtx)

	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	adminEmail := "browse-test-" + uuid.NewString() + "@sirfi.test"
	adminID, err := adminStore.CreateAdmin(ctx, adminEmail, "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("provision admin: %v", err)
	}

	var loginResp struct {
		Token string `json:"token"`
	}
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/login", "", map[string]string{
		"email": adminEmail, "password": "correct-horse-battery-staple",
	}, &loginResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: expected 200, got %d", resp.StatusCode)
	}
	token := loginResp.Token

	// --- Tenants: create two, then list/filter/paginate/detail ---
	var tenantIDs []string
	for range 2 {
		var createResp struct {
			ID string `json:"id"`
		}
		resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/tenants", token, map[string]string{
			"name": "Browse Test Tenant " + uuid.NewString(), "email": "browse-tenant-" + uuid.NewString() + "@sirfi.test",
		}, &createResp)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create tenant: expected 201, got %d", resp.StatusCode)
		}
		tenantIDs = append(tenantIDs, createResp.ID)
	}

	type tenantItem struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	var tenantList struct {
		Items  []tenantItem `json:"items"`
		Limit  int          `json:"limit"`
		Offset int          `json:"offset"`
		Total  int          `json:"total"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/tenants", token, nil, &tenantList)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tenants: expected 200, got %d", resp.StatusCode)
	}
	if tenantList.Total < 2 {
		t.Fatalf("expected at least the 2 tenants just created, got total=%d", tenantList.Total)
	}
	foundBoth := 0
	for _, item := range tenantList.Items {
		for _, id := range tenantIDs {
			if item.ID == id {
				foundBoth++
			}
		}
	}
	// Both may not fit in the default page if other tests seeded many rows —
	// only assert the pagination knob itself works, and detail lookups below
	// confirm the individual records are actually reachable.
	if tenantList.Limit != defaultPageLimit {
		t.Fatalf("expected default limit %d, got %d", defaultPageLimit, tenantList.Limit)
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/tenants?limit=1", token, nil, &tenantList)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tenants (limit=1): expected 200, got %d", resp.StatusCode)
	}
	if len(tenantList.Items) != 1 {
		t.Fatalf("expected exactly 1 item with limit=1, got %d", len(tenantList.Items))
	}
	if tenantList.Total < 2 {
		t.Fatalf("expected total to reflect the full count regardless of limit, got %d", tenantList.Total)
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/tenants?status=pending_kyb", token, nil, &tenantList)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tenants (status filter): expected 200, got %d", resp.StatusCode)
	}
	for _, item := range tenantList.Items {
		if item.Status != "pending_kyb" {
			t.Fatalf("status filter leaked a %s tenant", item.Status)
		}
	}

	var tenantDetail tenantItem
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/tenants/"+tenantIDs[0], token, nil, &tenantDetail)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get tenant: expected 200, got %d", resp.StatusCode)
	}
	if tenantDetail.ID != tenantIDs[0] {
		t.Fatalf("get tenant: expected id %s, got %s", tenantIDs[0], tenantDetail.ID)
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/tenants/not-a-uuid", token, nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("get tenant (malformed id): expected 400, got %d", resp.StatusCode)
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/tenants/"+uuid.NewString(), token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get tenant (unknown id): expected 404, got %d", resp.StatusCode)
	}

	if foundBoth == 0 {
		// Sanity: at least confirm the two just-created tenants are
		// independently reachable by ID even if page 1 didn't happen to
		// contain both.
		var d tenantItem
		resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/tenants/"+tenantIDs[1], token, nil, &d)
		if resp.StatusCode != http.StatusOK || d.ID != tenantIDs[1] {
			t.Fatalf("expected second created tenant to be independently reachable, got status %d", resp.StatusCode)
		}
	}

	// --- Corridors: create one, then list/filter/detail ---
	corridorStore := corridor.New(pool)
	corridorID, err := corridorStore.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:   "USDT",
		CryptoNetwork: "TESTNET_BROWSE_" + uuid.NewString(),
		FiatCurrency:  "NGN",
		Active:        true,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}

	type corridorItem struct {
		ID     string `json:"id"`
		Active bool   `json:"active"`
	}
	var corridorList struct {
		Items []corridorItem `json:"items"`
		Total int            `json:"total"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/corridors?active=true", token, nil, &corridorList)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list corridors: expected 200, got %d", resp.StatusCode)
	}
	found := false
	for _, item := range corridorList.Items {
		if item.ID == corridorID.String() {
			found = true
			if !item.Active {
				t.Fatal("expected the created corridor to be active")
			}
		}
	}
	if !found {
		t.Fatal("expected the created corridor to appear in the active-only list")
	}

	var corridorDetail corridorItem
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/corridors/"+corridorID.String(), token, nil, &corridorDetail)
	if resp.StatusCode != http.StatusOK || corridorDetail.ID != corridorID.String() {
		t.Fatalf("get corridor: expected 200 with matching id, got status %d", resp.StatusCode)
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/corridors/"+uuid.NewString(), token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get corridor (unknown id): expected 404, got %d", resp.StatusCode)
	}

	// --- Sessions: fresh tenant has none; unknown session id 404s ---
	var sessionList struct {
		Items []any `json:"items"`
		Total int   `json:"total"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/tenants/"+tenantIDs[0]+"/sessions", token, nil, &sessionList)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list tenant sessions: expected 200, got %d", resp.StatusCode)
	}
	if sessionList.Total != 0 || len(sessionList.Items) != 0 {
		t.Fatalf("expected a freshly created tenant to have zero sessions, got total=%d items=%d", sessionList.Total, len(sessionList.Items))
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/sessions/"+uuid.NewString(), token, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get session (unknown id): expected 404, got %d", resp.StatusCode)
	}

	// --- admin_audit_log: LogAction isn't called by any handler yet (a
	// pre-existing gap, not this phase's scope — see
	// docs/IMPLEMENTATION_PLAN.md's Phase 11 write-up), so write a row
	// directly at the store level, same convention onboarding_test.go uses
	// for corridor.UpsertCorridor, to prove the read path itself works.
	if err := adminStore.LogAction(ctx, adminID, "browse_test_action", "target-1", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("log admin action: %v", err)
	}

	type auditItem struct {
		AdminID string `json:"admin_id"`
		Action  string `json:"action"`
	}
	var auditList struct {
		Items []auditItem `json:"items"`
		Total int         `json:"total"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/audit-log?admin_id="+adminID.String(), token, nil, &auditList)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list audit log: expected 200, got %d", resp.StatusCode)
	}
	foundAction := false
	for _, item := range auditList.Items {
		if item.Action == "browse_test_action" {
			foundAction = true
		}
		if item.AdminID != adminID.String() {
			t.Fatalf("admin_id filter leaked a row from admin %s", item.AdminID)
		}
	}
	if !foundAction {
		t.Fatal("expected the logged action to appear in this admin's filtered audit log")
	}

	// --- request_audit_log: every request made above already generated
	// rows via the outermost audit middleware — confirm the read path
	// surfaces at least this test's own admin-login request.
	type reqLogItem struct {
		Path     string  `json:"path"`
		AdminID  *string `json:"admin_id,omitempty"`
		BodyHash string  `json:"body_hash,omitempty"`
	}
	var reqLogList struct {
		Items []reqLogItem `json:"items"`
		Total int          `json:"total"`
	}
	// Logger.Run drains its channel asynchronously — poll rather than assert
	// immediately, same pattern session_test.go's waitForHTTPStatus uses for
	// the eventbus dispatcher's own async effects.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/request-audit-log?admin_id="+adminID.String(), token, nil, &reqLogList)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list request audit log: expected 200, got %d", resp.StatusCode)
		}
		if reqLogList.Total > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if reqLogList.Total == 0 {
		t.Fatal("expected at least one request-audit-log row for this admin's own requests")
	}
	for _, item := range reqLogList.Items {
		if item.AdminID == nil || *item.AdminID != adminID.String() {
			t.Fatalf("admin_id filter leaked a row not belonging to admin %s", adminID.String())
		}
		if item.BodyHash != "" {
			t.Fatal("expected body_hash to never be surfaced in the response shape")
		}
	}

	// --- Access control: every new route must still reject unauthenticated
	// requests, same as every other /admin route.
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/tenants", "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated tenant list, got %d", resp.StatusCode)
	}
}
