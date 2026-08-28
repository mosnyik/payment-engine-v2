package adminauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig points at one IdP for admin staff login (Phase 13). Generic
// OIDC — any compliant IdP (Google Workspace, Okta, Azure AD, ...) works
// through this without code changes, just IdP-side app registration.
type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// ErrOIDCNonceMismatch means the ID token's nonce claim didn't match the
// one issued at the start of this login attempt — either a replayed token
// or the state/nonce cookie didn't round-trip correctly.
var ErrOIDCNonceMismatch = errors.New("adminauth: oidc nonce mismatch")

// OIDCAuthenticator wraps IdP discovery, the authorization-code exchange,
// and ID token verification. Built once at startup (discovery is a network
// call) — construction failure should fail app startup, same as any other
// required-at-boot dependency.
type OIDCAuthenticator struct {
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
}

// NewOIDCAuthenticator performs OIDC discovery against cfg.IssuerURL and
// builds an authenticator. Returns an error if the issuer can't be reached
// or doesn't serve a valid discovery document.
func NewOIDCAuthenticator(ctx context.Context, cfg OIDCConfig) (*OIDCAuthenticator, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("adminauth: oidc discovery: %w", err)
	}

	return &OIDCAuthenticator{
		oauth2Config: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

// AuthCodeURL builds the redirect-to-IdP URL for one login attempt. state
// and nonce are caller-generated (see admin_oidc_handlers.go) and must be
// round-tripped through the callback to be checked by Exchange.
func (a *OIDCAuthenticator) AuthCodeURL(state, nonce string) string {
	return a.oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce))
}

// Exchange trades an authorization code for an ID token, verifies it
// (issuer, audience, expiry — via the discovery-configured verifier — plus
// the nonce, checked here against expectedNonce), and returns the token's
// subject (`sub`, the stable identity to bind admin_users.oidc_subject to)
// and email claims.
func (a *OIDCAuthenticator) Exchange(ctx context.Context, code, expectedNonce string) (subject, email string, err error) {
	oauth2Token, err := a.oauth2Config.Exchange(ctx, code)
	if err != nil {
		return "", "", fmt.Errorf("adminauth: oidc code exchange: %w", err)
	}

	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return "", "", errors.New("adminauth: oidc token response missing id_token")
	}

	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return "", "", fmt.Errorf("adminauth: oidc id token verification: %w", err)
	}

	if idToken.Nonce != expectedNonce {
		return "", "", ErrOIDCNonceMismatch
	}

	var claims struct {
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", "", fmt.Errorf("adminauth: oidc claims: %w", err)
	}

	return idToken.Subject, claims.Email, nil
}
