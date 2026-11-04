package tenant_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

// fakeEmailSender captures the last email it was asked to send — used to
// pull the raw magic-link token out of the body without needing a real
// email provider configured, same "capture what got sent to the fake"
// testing style this codebase already uses for other outbound integrations.
type fakeEmailSender struct {
	enabled bool
	sendErr error

	lastTo, lastSubject, lastBody string
}

func (f *fakeEmailSender) IsEnabled() bool { return f.enabled }

func (f *fakeEmailSender) Send(ctx context.Context, to, subject, body string) error {
	f.lastTo, f.lastSubject, f.lastBody = to, subject, body
	return f.sendErr
}

// testPortalOrigin is the origin newStore allowlists; testPortalURL is a
// portal_url under it, with a path, as a real frontend would send.
const (
	testPortalOrigin = "https://portal.example.test"
	testPortalURL    = testPortalOrigin + "/dashboard"
)

// extractMagicLink pulls the emailed "{portal_url}/verify?token=..." link
// out of a magic-link email body.
func extractMagicLink(t *testing.T, body string) *url.URL {
	t.Helper()
	const prefix = "Use this link to sign in: "
	_, rest, ok := strings.Cut(body, prefix)
	if !ok {
		t.Fatalf("email body missing expected prefix: %q", body)
	}
	raw, _, _ := strings.Cut(rest, "\n")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse emailed link %q: %v", raw, err)
	}
	return u
}

func extractMagicLinkToken(t *testing.T, body string) string {
	t.Helper()
	token := extractMagicLink(t, body).Query().Get("token")
	if token == "" {
		t.Fatalf("extracted empty token from body: %q", body)
	}
	return token
}

func TestRegisterTenant_CreatesPendingKYBTenantWithEmail(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	email := "owner+" + uuid.NewString() + "@example.com"
	id, err := s.RegisterTenant(ctx, "Test Bank "+uuid.NewString(), email)
	if err != nil {
		t.Fatalf("register tenant: %v", err)
	}

	got, err := s.GetTenant(ctx, id)
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if got.Status != tenant.StatusPendingKYB {
		t.Fatalf("expected status pending_kyb, got %s", got.Status)
	}
}

func TestRegisterTenant_DuplicateEmailRejected(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	email := "owner+" + uuid.NewString() + "@example.com"
	if _, err := s.RegisterTenant(ctx, "Test Bank A", email); err != nil {
		t.Fatalf("first register: %v", err)
	}

	_, err := s.RegisterTenant(ctx, "Test Bank B", email)
	if !errors.Is(err, tenant.ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken on duplicate email, got %v", err)
	}
}

func TestRequestMagicLink_UnknownEmail(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	err := s.RequestMagicLink(ctx, "nobody+"+uuid.NewString()+"@example.com", testPortalURL)
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for an unregistered email, got %v", err)
	}
}

func TestRequestMagicLink_NoEmailSenderConfigured(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	email := "owner+" + uuid.NewString() + "@example.com"
	if _, err := s.RegisterTenant(ctx, "Test Bank", email); err != nil {
		t.Fatalf("register tenant: %v", err)
	}

	// No SetEmailSender call at all — same "fails safe, never silently
	// pretends it worked" reasoning notification's disabled-provider path
	// already establishes.
	err := s.RequestMagicLink(ctx, email, testPortalURL)
	if !errors.Is(err, tenant.ErrEmailDeliveryUnavailable) {
		t.Fatalf("expected ErrEmailDeliveryUnavailable with no sender configured, got %v", err)
	}
}

func TestMagicLinkFlow_RequestVerifyThenAuthenticate(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)
	sender := &fakeEmailSender{enabled: true}
	s.SetEmailSender(sender)

	email := "owner+" + uuid.NewString() + "@example.com"
	tenantID, err := s.RegisterTenant(ctx, "Test Bank", email)
	if err != nil {
		t.Fatalf("register tenant: %v", err)
	}

	if err := s.RequestMagicLink(ctx, email, testPortalURL); err != nil {
		t.Fatalf("request magic link: %v", err)
	}
	if sender.lastTo != email {
		t.Fatalf("expected the link to be sent to %q, sent to %q", email, sender.lastTo)
	}
	token := extractMagicLinkToken(t, sender.lastBody)

	sessionToken, gotTenantID, err := s.VerifyMagicLink(ctx, token)
	if err != nil {
		t.Fatalf("verify magic link: %v", err)
	}
	if gotTenantID != tenantID {
		t.Fatalf("expected tenant id %s, got %s", tenantID, gotTenantID)
	}
	if sessionToken == "" {
		t.Fatal("expected a non-empty session token")
	}

	gotTenantID, err = s.AuthenticateSession(ctx, sessionToken)
	if err != nil {
		t.Fatalf("authenticate session: %v", err)
	}
	if gotTenantID != tenantID {
		t.Fatalf("expected tenant id %s, got %s", tenantID, gotTenantID)
	}
}

func TestVerifyMagicLink_CannotBeReused(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)
	sender := &fakeEmailSender{enabled: true}
	s.SetEmailSender(sender)

	email := "owner+" + uuid.NewString() + "@example.com"
	if _, err := s.RegisterTenant(ctx, "Test Bank", email); err != nil {
		t.Fatalf("register tenant: %v", err)
	}
	if err := s.RequestMagicLink(ctx, email, testPortalURL); err != nil {
		t.Fatalf("request magic link: %v", err)
	}
	token := extractMagicLinkToken(t, sender.lastBody)

	if _, _, err := s.VerifyMagicLink(ctx, token); err != nil {
		t.Fatalf("first verify: %v", err)
	}

	_, _, err := s.VerifyMagicLink(ctx, token)
	if !errors.Is(err, tenant.ErrInvalidMagicLink) {
		t.Fatalf("expected ErrInvalidMagicLink on reuse, got %v", err)
	}
}

func TestVerifyMagicLink_UnknownToken(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	_, _, err := s.VerifyMagicLink(ctx, "not-a-real-token")
	if !errors.Is(err, tenant.ErrInvalidMagicLink) {
		t.Fatalf("expected ErrInvalidMagicLink for an unknown token, got %v", err)
	}
}

func TestAuthenticateSession_UnknownToken(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	_, err := s.AuthenticateSession(ctx, "not-a-real-session-token")
	if !errors.Is(err, tenant.ErrPortalSessionInvalid) {
		t.Fatalf("expected ErrPortalSessionInvalid for an unknown token, got %v", err)
	}
}

func TestListAPIKeys_And_RevokeAPIKey(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	tenantID, err := s.CreateTenant(ctx, "Test Bank "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := s.ApproveKYB(ctx, tenantID); err != nil {
		t.Fatalf("approve kyb: %v", err)
	}

	apiKey1, _, err := s.IssueAPIKey(ctx, tenantID)
	if err != nil {
		t.Fatalf("issue api key 1: %v", err)
	}
	apiKey2, _, err := s.IssueAPIKey(ctx, tenantID)
	if err != nil {
		t.Fatalf("issue api key 2: %v", err)
	}

	keys, err := s.ListAPIKeys(ctx, tenantID)
	if err != nil {
		t.Fatalf("list api keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	// Newest first.
	if keys[0].APIKey != apiKey2 || keys[1].APIKey != apiKey1 {
		t.Fatalf("expected newest-first ordering, got %+v", keys)
	}
	for _, k := range keys {
		if !k.Active || k.RevokedAt != nil {
			t.Fatalf("expected a freshly issued key to be active with no revoked_at, got %+v", k)
		}
	}

	if err := s.RevokeAPIKey(ctx, tenantID, keys[1].ID); err != nil {
		t.Fatalf("revoke api key: %v", err)
	}

	_, _, _, _, _, _, ok, err := s.LookupHMACSecret(ctx, apiKey1)
	if err != nil {
		t.Fatalf("lookup after revoke: %v", err)
	}
	if ok {
		t.Fatal("expected a revoked key to no longer authenticate")
	}

	// Revoking again (already revoked) and revoking a key ID under a
	// different tenant both resolve to ErrNotFound — the RevokeAPIKey doc
	// comment's "indistinguishable" contract.
	if err := s.RevokeAPIKey(ctx, tenantID, keys[1].ID); !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("expected ErrNotFound revoking an already-revoked key, got %v", err)
	}

	otherTenantID, err := s.CreateTenant(ctx, "Other Bank "+uuid.NewString())
	if err != nil {
		t.Fatalf("create other tenant: %v", err)
	}
	if err := s.RevokeAPIKey(ctx, otherTenantID, keys[0].ID); !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("expected ErrNotFound revoking another tenant's key, got %v", err)
	}
}

func TestValidatePortalURL(t *testing.T) {
	s, err := tenant.New(nil, make([]byte, 32))
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}
	s.SetAllowedPortalOrigins([]string{"https://portal.example.test", "http://localhost:3000/"})

	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"origin only", "https://portal.example.test", true},
		{"with path", "https://portal.example.test/dashboard/auth", true},
		{"trailing slash", "https://portal.example.test/", true},
		{"origin match is case-insensitive", "https://PORTAL.example.test/x", true},
		{"allowed localhost with port", "http://localhost:3000/app", true},
		{"empty", "", false},
		{"relative", "/dashboard", false},
		{"not allowlisted", "https://evil.example/dashboard", false},
		{"lookalike subdomain", "https://portal.example.test.evil.example", false},
		{"wrong scheme for allowed host", "http://portal.example.test", false},
		{"wrong port", "http://localhost:4000", false},
		{"userinfo trick", "https://portal.example.test@evil.example", false},
		{"credentials", "https://user:pass@portal.example.test", false},
		{"query", "https://portal.example.test/x?next=1", false},
		{"fragment", "https://portal.example.test/x#y", false},
		{"javascript scheme", "javascript:alert(1)", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.ValidatePortalURL(tc.raw)
			if tc.ok && err != nil {
				t.Fatalf("expected %q to be accepted, got %v", tc.raw, err)
			}
			if !tc.ok && !errors.Is(err, tenant.ErrInvalidPortalURL) {
				t.Fatalf("expected ErrInvalidPortalURL for %q, got %v", tc.raw, err)
			}
		})
	}
}

func TestValidatePortalURL_NoAllowlistRejectsEverything(t *testing.T) {
	s, err := tenant.New(nil, make([]byte, 32))
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}
	if _, err := s.ValidatePortalURL("https://portal.example.test"); !errors.Is(err, tenant.ErrInvalidPortalURL) {
		t.Fatalf("expected ErrInvalidPortalURL with no allowlist configured, got %v", err)
	}
}

func TestRequestMagicLink_InvalidPortalURLCheckedBeforeLookup(t *testing.T) {
	pool := openTestPool(t)
	s := newStore(t, pool)

	// An unregistered email with a bad portal_url must fail on the URL,
	// not ErrNotFound — the URL check can't leak whether an email exists.
	err := s.RequestMagicLink(context.Background(), "nobody+"+uuid.NewString()+"@example.com", "https://evil.example")
	if !errors.Is(err, tenant.ErrInvalidPortalURL) {
		t.Fatalf("expected ErrInvalidPortalURL, got %v", err)
	}
}

func TestRequestMagicLink_LinkBuiltFromPortalURL(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)
	sender := &fakeEmailSender{enabled: true}
	s.SetEmailSender(sender)

	email := "owner+" + uuid.NewString() + "@example.com"
	if _, err := s.RegisterTenant(ctx, "Test Bank", email); err != nil {
		t.Fatalf("register tenant: %v", err)
	}
	if err := s.RequestMagicLink(ctx, email, testPortalURL+"/"); err != nil {
		t.Fatalf("request magic link: %v", err)
	}

	link := extractMagicLink(t, sender.lastBody)
	if got, want := link.Scheme+"://"+link.Host+link.Path, testPortalURL+"/verify"; got != want {
		t.Fatalf("expected link to start with %q, got %q", want, got)
	}
	if link.Query().Get("token") == "" {
		t.Fatalf("expected a token query parameter in %q", link)
	}
}
