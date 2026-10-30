// Phase 9a acceptance test: drives the tenant self-service portal
// (register -> verify magic link -> dashboard) through real HTTP requests
// against the actual router, the same style onboarding_test.go established
// for the admin-driven flow. Proves the portal auth (internal/tenant),
// tenant-scoped reads (session/settlement/notification/ledger), and
// self-service credential management modules actually compose end to end.
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

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
)

// fakePortalEmailSender captures the last email it was asked to send —
// same "capture what got sent to the fake" style internal/tenant's own
// portal_test.go uses, so the raw magic-link token can be pulled out of
// the body without a real email provider configured.
type fakePortalEmailSender struct {
	lastTo, lastBody string
}

func (f *fakePortalEmailSender) IsEnabled() bool { return true }

func (f *fakePortalEmailSender) Send(ctx context.Context, to, subject, body string) error {
	f.lastTo, f.lastBody = to, body
	return nil
}

func extractPortalMagicLinkToken(t *testing.T, body string) string {
	t.Helper()
	const prefix = "Use this link to sign in: "
	_, rest, ok := strings.Cut(body, prefix)
	if !ok {
		t.Fatalf("email body missing expected prefix: %q", body)
	}
	token, _, _ := strings.Cut(rest, "\n")
	if token == "" {
		t.Fatalf("extracted empty token from body: %q", body)
	}
	return token
}

func TestPortalWorkflowEndToEnd(t *testing.T) {
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
	// buildStores wires a real (disabled, in this test config) email
	// provider — swap in a fake so the magic-link token can be captured
	// instead of requiring an actual email provider configured.
	sender := &fakePortalEmailSender{}
	stores.tenant.SetEmailSender(sender)

	router, err := buildRouter(cfg, stores)
	if err != nil {
		t.Fatalf("build router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	client := srv.Client()

	// Out-of-band provisioning, same as onboarding_test.go: an admin
	// account to resolve the KYB hold portal submission lands in (no
	// screening provider registered, same as the admin-driven flow).
	adminStore := adminauth.New(pool, cfg.AdminSessionTTL)
	adminEmail := "portal-test-" + uuid.NewString() + "@sirfi.test"
	if _, err := adminStore.CreateAdmin(ctx, adminEmail, "correct-horse-battery-staple"); err != nil {
		t.Fatalf("provision admin: %v", err)
	}
	var adminLoginResp struct {
		Token string `json:"token"`
	}
	resp := doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/login", "", map[string]string{
		"email": adminEmail, "password": "correct-horse-battery-staple",
	}, &adminLoginResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login: expected 200, got %d", resp.StatusCode)
	}
	adminToken := adminLoginResp.Token

	// --- Register ---
	tenantEmail := "owner+" + uuid.NewString() + "@example.com"
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/register", "", map[string]string{
		"name": "Test Fintech " + uuid.NewString(), "email": tenantEmail,
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("register: expected 202, got %d", resp.StatusCode)
	}
	if sender.lastTo != tenantEmail {
		t.Fatalf("expected magic link sent to %q, sent to %q", tenantEmail, sender.lastTo)
	}
	token := extractPortalMagicLinkToken(t, sender.lastBody)

	// A raw, unverified magic link must not itself grant dashboard access —
	// only a redeemed session token does.
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/me", token, nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected a magic-link token to be rejected as a session token, got %d", resp.StatusCode)
	}

	// --- Verify: redeem the magic link for a real session token ---
	var verifyResp struct {
		Token string `json:"token"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/verify?token="+url.QueryEscape(token), "", nil, &verifyResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify: expected 200, got %d", resp.StatusCode)
	}
	sessionToken := verifyResp.Token
	if sessionToken == "" {
		t.Fatal("verify: expected a non-empty session token")
	}

	// The same magic link can't be redeemed twice.
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/verify?token="+url.QueryEscape(token), "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected re-verifying a consumed magic link to fail, got %d", resp.StatusCode)
	}

	// --- Dashboard: pending_kyb before KYB clears ---
	var meResp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/me", sessionToken, nil, &meResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get me: expected 200, got %d", resp.StatusCode)
	}
	if meResp.Status != "pending_kyb" {
		t.Fatalf("expected status pending_kyb, got %s", meResp.Status)
	}
	tenantID := meResp.ID

	// Credential issuance before KYB approval must be refused.
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/api-keys", sessionToken, nil, nil)
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("expected credential issuance to be refused before KYB approval")
	}

	// --- Self-service KYB submission: provider_name is never client-
	// controlled, so this lands in the hold queue exactly like the
	// admin-submitted path in onboarding_test.go. ---
	var kybCase struct {
		ID     string `json:"ID"`
		Status string `json:"Status"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/kyb", sessionToken, map[string]any{
		"submitted_data":      map[string]string{"company": "Test Fintech Ltd"},
		"declared_currencies": []string{"NGN"},
	}, &kybCase)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit kyb: expected 201, got %d", resp.StatusCode)
	}
	if compliance.Status(kybCase.Status) != compliance.StatusHold {
		t.Fatalf("expected kyb case to land in hold, got %s", kybCase.Status)
	}

	// A self-service submission that tried to set provider_name would be
	// ignored server-side regardless — proven implicitly: the case above
	// landed in hold even though no provider_name field exists on the
	// request struct decoded by portalHandlers.submitKYB at all.

	// --- Admin resolves the hold (same admin-facing surface onboarding
	// uses; the portal has no self-approval path). ---
	var resolvedCase struct {
		Status string `json:"Status"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/admin/compliance/holds/"+kybCase.ID+"/resolve", adminToken, map[string]any{
		"approved": true, "reason": "documents verified",
	}, &resolvedCase)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve hold: expected 200, got %d", resp.StatusCode)
	}
	if compliance.Status(resolvedCase.Status) != compliance.StatusApproved {
		t.Fatalf("expected resolved case to be approved, got %s", resolvedCase.Status)
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/me", sessionToken, nil, &meResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get me after approval: expected 200, got %d", resp.StatusCode)
	}
	if meResp.Status != "active" {
		t.Fatalf("expected status active after kyb approval, got %s", meResp.Status)
	}

	// --- Self-service API key issuance, listing, and revocation ---
	var credsResp struct {
		APIKey     string `json:"api_key"`
		HMACSecret string `json:"hmac_secret"`
	}
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/api-keys", sessionToken, nil, &credsResp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue api key: expected 201, got %d", resp.StatusCode)
	}
	if credsResp.APIKey == "" || credsResp.HMACSecret == "" {
		t.Fatal("expected non-empty api key and hmac secret")
	}

	var listedKeys []struct {
		ID     string `json:"id"`
		APIKey string `json:"api_key"`
		Active bool   `json:"active"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/api-keys", sessionToken, nil, &listedKeys)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list api keys: expected 200, got %d", resp.StatusCode)
	}
	if len(listedKeys) != 1 || listedKeys[0].APIKey != credsResp.APIKey || !listedKeys[0].Active {
		t.Fatalf("expected exactly one active listed key matching the issued one, got %+v", listedKeys)
	}

	// Final proof: the issued credential authenticates on a real protected
	// tenant-facing route (/v2/ping) — same proof onboarding_test.go
	// establishes for the admin-issued credential.
	timestampStr := jsonNumberString(time.Now().UnixMilli())
	sig := gateway.Sign(credsResp.HMACSecret, http.MethodGet, "/v2/ping", timestampStr, nil)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/ping", nil)
	req.Header.Set("X-API-Key", credsResp.APIKey)
	req.Header.Set("X-Timestamp", timestampStr)
	req.Header.Set("X-Signature", sig)
	pingResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("ping with issued credential: %v", err)
	}
	pingResp.Body.Close()
	if pingResp.StatusCode != http.StatusOK {
		t.Fatalf("expected /v2/ping with the portal-issued credential to return 200, got %d", pingResp.StatusCode)
	}

	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/api-keys/"+listedKeys[0].ID+"/revoke", sessionToken, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke api key: expected 200, got %d", resp.StatusCode)
	}

	timestampStr2 := jsonNumberString(time.Now().UnixMilli())
	sig2 := gateway.Sign(credsResp.HMACSecret, http.MethodGet, "/v2/ping", timestampStr2, nil)
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/ping", nil)
	req2.Header.Set("X-API-Key", credsResp.APIKey)
	req2.Header.Set("X-Timestamp", timestampStr2)
	req2.Header.Set("X-Signature", sig2)
	pingResp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("ping with revoked credential: %v", err)
	}
	pingResp2.Body.Close()
	if pingResp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected /v2/ping with a revoked credential to return 401, got %d", pingResp2.StatusCode)
	}

	// --- Tenant-scoped reads: nothing created yet, so all empty/zero, not
	// an error and never another tenant's data. ---
	var sessions []map[string]any
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/sessions", sessionToken, nil, &sessions)
	if resp.StatusCode != http.StatusOK || len(sessions) != 0 {
		t.Fatalf("expected empty sessions list (200), got status %d body %v", resp.StatusCode, sessions)
	}

	var settlements []map[string]any
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/settlements", sessionToken, nil, &settlements)
	if resp.StatusCode != http.StatusOK || len(settlements) != 0 {
		t.Fatalf("expected empty settlements list (200), got status %d body %v", resp.StatusCode, settlements)
	}

	var notifications []map[string]any
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/notifications", sessionToken, nil, &notifications)
	if resp.StatusCode != http.StatusOK || len(notifications) != 0 {
		t.Fatalf("expected empty notifications list (200), got status %d body %v", resp.StatusCode, notifications)
	}

	var balanceResp struct {
		TenantPayable string `json:"tenant_payable"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/balance?fiat_currency=NGN", sessionToken, nil, &balanceResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get balance: expected 200, got %d", resp.StatusCode)
	}
	if balanceResp.TenantPayable != "0" {
		t.Fatalf("expected zero balance for a tenant with no ledger activity, got %q", balanceResp.TenantPayable)
	}

	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/balance", sessionToken, nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a balance request missing fiat_currency, got %d", resp.StatusCode)
	}

	// --- Self-service webhook config, SSRF-validated same as the admin path ---
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/v2/portal/webhook", sessionToken, map[string]string{
		"url": "https://169.254.169.254/latest/meta-data/",
	}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected the SSRF webhook attempt to be rejected with 400, got %d", resp.StatusCode)
	}

	var webhookResp struct {
		WebhookSigningSecret string `json:"webhook_signing_secret"`
	}
	resp = doJSON(t, client, http.MethodPut, srv.URL+"/v2/portal/webhook", sessionToken, map[string]string{
		"url": "https://example.com/webhooks/sirfi",
	}, &webhookResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set webhook url: expected 200, got %d", resp.StatusCode)
	}
	if webhookResp.WebhookSigningSecret == "" {
		t.Fatal("expected a webhook signing secret on first-time setup")
	}

	// --- Cross-tenant isolation: a second tenant's portal session must
	// never see the first tenant's data. ---
	otherEmail := "owner+" + uuid.NewString() + "@example.com"
	resp = doJSON(t, client, http.MethodPost, srv.URL+"/v2/portal/register", "", map[string]string{
		"name": "Other Fintech " + uuid.NewString(), "email": otherEmail,
	}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("register other tenant: expected 202, got %d", resp.StatusCode)
	}
	otherToken := extractPortalMagicLinkToken(t, sender.lastBody)
	var otherVerifyResp struct {
		Token string `json:"token"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/verify?token="+url.QueryEscape(otherToken), "", nil, &otherVerifyResp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("verify other tenant: expected 200, got %d", resp.StatusCode)
	}

	var otherKeys []map[string]any
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/api-keys", otherVerifyResp.Token, nil, &otherKeys)
	if resp.StatusCode != http.StatusOK || len(otherKeys) != 0 {
		t.Fatalf("expected the other tenant to see zero api keys (never the first tenant's), got status %d body %v", resp.StatusCode, otherKeys)
	}

	var otherMe struct {
		ID string `json:"id"`
	}
	resp = doJSON(t, client, http.MethodGet, srv.URL+"/v2/portal/me", otherVerifyResp.Token, nil, &otherMe)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get other tenant me: expected 200, got %d", resp.StatusCode)
	}
	if otherMe.ID == tenantID {
		t.Fatal("expected the other tenant's session to resolve to a different tenant id")
	}
}
