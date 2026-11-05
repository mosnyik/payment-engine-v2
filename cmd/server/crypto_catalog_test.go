// Admin CRUD over the supported crypto catalog (/v2/admin/crypto/*), and
// POST /v2/admin/corridors only accepting what the catalog supports.
package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAdminCryptoCatalogCRUD(t *testing.T) {
	env := newCorridorAdminTestEnv(t)
	srv, client, token := env.srv, env.client, env.token
	call := func(method, path string, body, out any) int {
		t.Helper()
		return doJSON(t, client, method, srv.URL+"/v2/admin/crypto"+path, token, body, out).StatusCode
	}
	suffix := strings.ToUpper(strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	net, asset := "NET"+suffix, "AST"+suffix

	type network struct {
		Code   string `json:"code"`
		Name   string `json:"name"`
		Active bool   `json:"active"`
	}
	type assetT struct {
		Code     string   `json:"code"`
		Name     string   `json:"name"`
		Active   bool     `json:"active"`
		Networks []string `json:"networks"`
	}

	// --- Seeded catalog is there, and admin-only ---
	var cat struct {
		Networks []network `json:"networks"`
		Assets   []assetT  `json:"assets"`
	}
	if code := call(http.MethodGet, "", nil, &cat); code != http.StatusOK {
		t.Fatalf("catalog: expected 200, got %d", code)
	}
	foundUSDT := false
	for _, a := range cat.Assets {
		if a.Code == "USDT" && strings.Contains(strings.Join(a.Networks, ","), "TRC20") {
			foundUSDT = true
		}
	}
	if !foundUSDT {
		t.Fatalf("seeded catalog should have USDT on TRC20: %+v", cat.Assets)
	}
	if code := doJSON(t, client, http.MethodGet, srv.URL+"/v2/admin/crypto", "", nil, nil).StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("catalog without a token: expected 401, got %d", code)
	}

	// --- Network create, validation, duplicate, read, update ---
	var n network
	if code := call(http.MethodPost, "/networks", map[string]any{"code": strings.ToLower(net), "name": "Test net"}, &n); code != http.StatusCreated {
		t.Fatalf("create network: expected 201, got %d", code)
	}
	if n.Code != net || !n.Active {
		t.Fatalf("create network: code should be uppercased and active by default, got %+v", n)
	}
	if code := call(http.MethodPost, "/networks", map[string]any{"code": net, "name": "again"}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate network: expected 409, got %d", code)
	}
	if code := call(http.MethodPost, "/networks", map[string]any{"code": "bad code!", "name": "x"}, nil); code != http.StatusBadRequest {
		t.Fatalf("invalid network code: expected 400, got %d", code)
	}
	if code := call(http.MethodPost, "/networks", map[string]any{"code": "OK" + suffix, "name": "  "}, nil); code != http.StatusBadRequest {
		t.Fatalf("blank network name: expected 400, got %d", code)
	}
	if code := call(http.MethodPatch, "/networks/"+net, map[string]any{"name": "Renamed"}, &n); code != http.StatusOK || n.Name != "Renamed" {
		t.Fatalf("rename network: got %d %+v", code, n)
	}
	if code := call(http.MethodPatch, "/networks/"+net, map[string]any{"code": "OTHER"}, nil); code != http.StatusBadRequest {
		t.Fatalf("change network code: expected 400, got %d", code)
	}
	if code := call(http.MethodGet, "/networks/"+strings.ToLower(net), nil, &n); code != http.StatusOK || n.Code != net {
		t.Fatalf("get network (any case): got %d %+v", code, n)
	}

	// --- Asset on that network; an unknown network is refused atomically ---
	var a assetT
	if code := call(http.MethodPost, "/assets", map[string]any{"code": asset, "name": "Test coin", "networks": []string{net, "NOPE" + suffix}}, nil); code != http.StatusBadRequest {
		t.Fatalf("asset on unknown network: expected 400, got %d", code)
	}
	if code := call(http.MethodGet, "/assets/"+asset, nil, nil); code != http.StatusNotFound {
		t.Fatalf("failed asset create must leave nothing behind: got %d", code)
	}
	if code := call(http.MethodPost, "/assets", map[string]any{"code": asset, "name": "Test coin", "networks": []string{strings.ToLower(net)}}, &a); code != http.StatusCreated {
		t.Fatalf("create asset: expected 201, got %d", code)
	}
	if len(a.Networks) != 1 || a.Networks[0] != net {
		t.Fatalf("create asset networks: %+v", a)
	}

	// --- Corridor create follows the catalog ---
	corridor := func(assetCode, networkCode string) int {
		t.Helper()
		return doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/corridors", token, map[string]any{
			"crypto_asset": assetCode, "crypto_network": networkCode, "fiat_currency": env.payoutCurrency(t),
			"providers": []map[string]any{{"type": "collection", "name": "busha"}, {"type": "settlement", "name": "paystack"}},
		}, nil).StatusCode
	}
	if code := corridor("USDT", net); code != http.StatusBadRequest {
		t.Fatalf("corridor for an asset not on that network: expected 400, got %d", code)
	}
	if code := corridor("NOSUCH"+suffix, net); code != http.StatusBadRequest {
		t.Fatalf("corridor for an unknown asset: expected 400, got %d", code)
	}
	if code := corridor(asset, net); code != http.StatusCreated {
		t.Fatalf("corridor for a supported pair: expected 201, got %d", code)
	}

	// A deactivated network takes no new corridors.
	if code := call(http.MethodPatch, "/networks/"+net, map[string]any{"active": false}, nil); code != http.StatusOK {
		t.Fatalf("deactivate network: got %d", code)
	}
	if code := corridor(asset, net); code != http.StatusBadRequest {
		t.Fatalf("corridor on a deactivated network: expected 400, got %d", code)
	}

	// --- In use: neither can be deleted ---
	if code := call(http.MethodDelete, "/networks/"+net, nil, nil); code != http.StatusConflict {
		t.Fatalf("delete network a corridor uses: expected 409, got %d", code)
	}
	if code := call(http.MethodDelete, "/assets/"+asset, nil, nil); code != http.StatusConflict {
		t.Fatalf("delete asset a corridor uses: expected 409, got %d", code)
	}

	// --- Asset update replaces networks; unused entries delete cleanly ---
	net2 := "NEU" + suffix
	if code := call(http.MethodPost, "/networks", map[string]any{"code": net2, "name": "Unused"}, nil); code != http.StatusCreated {
		t.Fatalf("create second network: got %d", code)
	}
	if code := call(http.MethodPatch, "/assets/"+asset, map[string]any{"networks": []string{net, net2}, "name": "Renamed coin"}, &a); code != http.StatusOK ||
		len(a.Networks) != 2 || a.Name != "Renamed coin" {
		t.Fatalf("update asset: got %d %+v", code, a)
	}
	if code := call(http.MethodDelete, "/networks/"+net2, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete unused network: expected 204, got %d", code)
	}
	if code := call(http.MethodGet, "/assets/"+asset, nil, &a); code != http.StatusOK || len(a.Networks) != 1 {
		t.Fatalf("deleting a network should drop it from assets: %+v", a)
	}
	if code := call(http.MethodDelete, "/networks/"+net2, nil, nil); code != http.StatusNotFound {
		t.Fatalf("delete network twice: expected 404, got %d", code)
	}
	if !env.hasAuditEntry(t, "crypto_network.delete", net2) {
		t.Fatal("expected a crypto_network.delete audit entry")
	}

	unused := "UNU" + suffix
	if code := call(http.MethodPost, "/assets", map[string]any{"code": unused, "name": "Unused coin"}, &a); code != http.StatusCreated || len(a.Networks) != 0 {
		t.Fatalf("create asset with no networks: got %d %+v", code, a)
	}
	if code := call(http.MethodDelete, "/assets/"+unused, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete unused asset: expected 204, got %d", code)
	}
}
