package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
)

// cryptoCatalogHandlers is admin CRUD over the supported crypto catalog —
// the networks and assets corridors may use (corridor/catalog.go):
//
//	GET    /admin/crypto                  both lists, for forms
//	GET    /admin/crypto/networks         POST /admin/crypto/networks
//	GET    /admin/crypto/networks/{code}  PATCH, DELETE
//	GET    /admin/crypto/assets           POST /admin/crypto/assets
//	GET    /admin/crypto/assets/{code}    PATCH, DELETE
type cryptoCatalogHandlers struct {
	corridor *corridor.Store
	admin    *adminauth.Store
}

type networkResponse struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type assetResponse struct {
	Code      string   `json:"code"`
	Name      string   `json:"name"`
	Active    bool     `json:"active"`
	Networks  []string `json:"networks"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func toNetworkResponse(n *corridor.Network) networkResponse {
	return networkResponse{Code: n.Code, Name: n.Name, Active: n.Active, CreatedAt: n.CreatedAt.Format(time.RFC3339), UpdatedAt: n.UpdatedAt.Format(time.RFC3339)}
}

func toAssetResponse(a *corridor.Asset) assetResponse {
	nets := a.Networks
	if nets == nil {
		nets = []string{}
	}
	return assetResponse{Code: a.Code, Name: a.Name, Active: a.Active, Networks: nets, CreatedAt: a.CreatedAt.Format(time.RFC3339), UpdatedAt: a.UpdatedAt.Format(time.RFC3339)}
}

// writeCatalogErr maps catalog store errors to HTTP statuses.
func writeCatalogErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, corridor.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, corridor.ErrAlreadyExists), errors.Is(err, corridor.ErrInUse):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, corridor.ErrInvalidCode), errors.Is(err, corridor.ErrInvalidName), errors.Is(err, corridor.ErrUnknownNetwork):
		writeErr(w, http.StatusBadRequest, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

func (h *cryptoCatalogHandlers) audit(r *http.Request, action, target string, metadata any) {
	if adminID, ok := adminauth.AdminIDFromContext(r.Context()); ok {
		if err := h.admin.LogAction(r.Context(), adminID, action, target, metadata); err != nil {
			log.Printf("admin: %s %s done but audit log write failed: %v", action, target, err)
		}
	}
}

// GET /admin/crypto — every network and asset, active or not.
func (h *cryptoCatalogHandlers) catalog(w http.ResponseWriter, r *http.Request) {
	networks, err := h.corridor.ListNetworks(r.Context())
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	assets, err := h.corridor.ListAssets(r.Context())
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	nr := make([]networkResponse, len(networks))
	for i := range networks {
		nr[i] = toNetworkResponse(&networks[i])
	}
	ar := make([]assetResponse, len(assets))
	for i := range assets {
		ar[i] = toAssetResponse(&assets[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": nr, "assets": ar})
}

// Networks -------------------------------------------------------------------

func (h *cryptoCatalogHandlers) listNetworks(w http.ResponseWriter, r *http.Request) {
	networks, err := h.corridor.ListNetworks(r.Context())
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	out := make([]networkResponse, len(networks))
	for i := range networks {
		out[i] = toNetworkResponse(&networks[i])
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *cryptoCatalogHandlers) getNetwork(w http.ResponseWriter, r *http.Request) {
	n, err := h.corridor.GetNetwork(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNetworkResponse(n))
}

// POST /admin/crypto/networks {"code": "TRC20", "name": "Tron (TRC-20)", "active": true}
func (h *cryptoCatalogHandlers) createNetwork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code   string `json:"code"`
		Name   string `json:"name"`
		Active *bool  `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := h.corridor.CreateNetwork(r.Context(), req.Code, req.Name, req.Active == nil || *req.Active)
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	resp := toNetworkResponse(n)
	h.audit(r, "crypto_network.create", n.Code, resp)
	writeJSON(w, http.StatusCreated, resp)
}

// PATCH /admin/crypto/networks/{code} {"name"?: "...", "active"?: bool}.
// The code is the identifier corridors store, so it can't be changed.
func (h *cryptoCatalogHandlers) updateNetwork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code   *string `json:"code"`
		Name   *string `json:"name"`
		Active *bool   `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	code := chi.URLParam(r, "code")
	if req.Code != nil && !strings.EqualFold(strings.TrimSpace(*req.Code), code) {
		writeErr(w, http.StatusBadRequest, errors.New("code can't be changed; create a new network instead"))
		return
	}
	if req.Name == nil && req.Active == nil {
		writeErr(w, http.StatusBadRequest, errors.New("no fields to update"))
		return
	}
	n, err := h.corridor.UpdateNetwork(r.Context(), code, req.Name, req.Active)
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	resp := toNetworkResponse(n)
	h.audit(r, "crypto_network.update", n.Code, resp)
	writeJSON(w, http.StatusOK, resp)
}

// DELETE /admin/crypto/networks/{code} — 409 while any corridor uses it.
func (h *cryptoCatalogHandlers) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(chi.URLParam(r, "code"))
	if err := h.corridor.DeleteNetwork(r.Context(), code); err != nil {
		writeCatalogErr(w, err)
		return
	}
	h.audit(r, "crypto_network.delete", code, map[string]string{"code": code})
	w.WriteHeader(http.StatusNoContent)
}

// Assets ---------------------------------------------------------------------

func (h *cryptoCatalogHandlers) listAssets(w http.ResponseWriter, r *http.Request) {
	assets, err := h.corridor.ListAssets(r.Context())
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	out := make([]assetResponse, len(assets))
	for i := range assets {
		out[i] = toAssetResponse(&assets[i])
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *cryptoCatalogHandlers) getAsset(w http.ResponseWriter, r *http.Request) {
	a, err := h.corridor.GetAsset(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAssetResponse(a))
}

// POST /admin/crypto/assets {"code": "USDT", "name": "Tether USD", "active": true, "networks": ["TRC20", "ERC20"]}
func (h *cryptoCatalogHandlers) createAsset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code     string   `json:"code"`
		Name     string   `json:"name"`
		Active   *bool    `json:"active"`
		Networks []string `json:"networks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	a, err := h.corridor.CreateAsset(r.Context(), req.Code, req.Name, req.Active == nil || *req.Active, req.Networks)
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	resp := toAssetResponse(a)
	h.audit(r, "crypto_asset.create", a.Code, resp)
	writeJSON(w, http.StatusCreated, resp)
}

// PATCH /admin/crypto/assets/{code} {"name"?, "active"?, "networks"?: [...]}
// — networks replaces the whole list.
func (h *cryptoCatalogHandlers) updateAsset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code     *string   `json:"code"`
		Name     *string   `json:"name"`
		Active   *bool     `json:"active"`
		Networks *[]string `json:"networks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	code := chi.URLParam(r, "code")
	if req.Code != nil && !strings.EqualFold(strings.TrimSpace(*req.Code), code) {
		writeErr(w, http.StatusBadRequest, errors.New("code can't be changed; create a new asset instead"))
		return
	}
	if req.Name == nil && req.Active == nil && req.Networks == nil {
		writeErr(w, http.StatusBadRequest, errors.New("no fields to update"))
		return
	}
	a, err := h.corridor.UpdateAsset(r.Context(), code, req.Name, req.Active, req.Networks)
	if err != nil {
		writeCatalogErr(w, err)
		return
	}
	resp := toAssetResponse(a)
	h.audit(r, "crypto_asset.update", a.Code, resp)
	writeJSON(w, http.StatusOK, resp)
}

// DELETE /admin/crypto/assets/{code} — 409 while any corridor uses it.
func (h *cryptoCatalogHandlers) deleteAsset(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(chi.URLParam(r, "code"))
	if err := h.corridor.DeleteAsset(r.Context(), code); err != nil {
		writeCatalogErr(w, err)
		return
	}
	h.audit(r, "crypto_asset.delete", code, map[string]string{"code": code})
	w.WriteHeader(http.StatusNoContent)
}
