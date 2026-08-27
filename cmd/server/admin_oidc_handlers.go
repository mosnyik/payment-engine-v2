package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
)

// adminOIDCStateCookie/adminOIDCNonceCookie hold this login attempt's CSRF
// state and replay-resistant nonce between the redirect and the callback.
// Httponly + SameSite=Lax (not Strict — Strict would drop the cookie on
// the IdP's top-level cross-site redirect back to us) + short TTL, so
// possession of the cookie is what proves "this callback belongs to a
// request we actually issued", the same job a server-side state store
// would do without needing one.
const (
	adminOIDCStateCookie  = "admin_oidc_state"
	adminOIDCNonceCookie  = "admin_oidc_nonce"
	adminOIDCCookieMaxAge = 600 // 10 minutes
)

// adminOIDCHandlers implements Phase 13's admin SSO login. Both routes are
// only registered (see router.go) when cfg.AdminOIDC.IssuerURL is set;
// oidc is never nil when these run.
type adminOIDCHandlers struct {
	oidc  *adminauth.OIDCAuthenticator
	admin *adminauth.Store
	// secureCookies mirrors cfg.IsProduction() — the state/nonce cookies
	// are marked Secure outside local dev, same as any other
	// security-relevant toggle in this codebase.
	secureCookies bool
}

// GET /admin/login/oidc — starts the login attempt: mint state+nonce, stash
// them in short-lived cookies, redirect to the IdP.
func (h *adminOIDCHandlers) login(w http.ResponseWriter, r *http.Request) {
	state, err := randomHex()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	nonce, err := randomHex()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	h.setShortLivedCookie(w, adminOIDCStateCookie, state)
	h.setShortLivedCookie(w, adminOIDCNonceCookie, nonce)

	http.Redirect(w, r, h.oidc.AuthCodeURL(state, nonce), http.StatusFound)
}

// GET /admin/login/oidc/callback?code=...&state=... — completes the login
// attempt: check state against the cookie (CSRF), exchange the code,
// verify the ID token (including nonce, against the other cookie), then
// resolve/bind the admin_users row and issue a session exactly like
// POST /admin/login does.
func (h *adminOIDCHandlers) callback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(adminOIDCStateCookie)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("missing or expired login state"))
		return
	}
	nonceCookie, err := r.Cookie(adminOIDCNonceCookie)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("missing or expired login state"))
		return
	}
	h.clearCookie(w, adminOIDCStateCookie)
	h.clearCookie(w, adminOIDCNonceCookie)

	if r.URL.Query().Get("state") != stateCookie.Value {
		writeErr(w, http.StatusBadRequest, errors.New("state mismatch"))
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		writeErr(w, http.StatusBadRequest, errors.New("missing code"))
		return
	}

	subject, email, err := h.oidc.Exchange(r.Context(), code, nonceCookie.Value)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}

	token, err := h.admin.LoginWithOIDC(r.Context(), subject, email)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (h *adminOIDCHandlers) setShortLivedCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   adminOIDCCookieMaxAge,
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *adminOIDCHandlers) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func randomHex() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
