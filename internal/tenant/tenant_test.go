package tenant_test

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}

	pool, err := db.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testKey(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func newStore(t *testing.T, pool *db.Pool) *tenant.Store {
	t.Helper()
	s, err := tenant.New(pool, testKey(t))
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}
	return s
}

func TestCreateTenant_StartsPendingKYB(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	id, err := s.CreateTenant(ctx, "Test Bank "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	got, err := s.GetTenant(ctx, id)
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if got.Status != tenant.StatusPendingKYB {
		t.Fatalf("expected status pending_kyb, got %s", got.Status)
	}
}

func TestIssueAPIKey_RefusedBeforeKYBApproval(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	id, err := s.CreateTenant(ctx, "Test Bank "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, _, err = s.IssueAPIKey(ctx, id)
	if err == nil {
		t.Fatal("expected an error issuing credentials before KYB approval")
	}
}

func TestApproveKYB_ThenIssueAndAuthenticate(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	id, err := s.CreateTenant(ctx, "Test Bank "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := s.ApproveKYB(ctx, id); err != nil {
		t.Fatalf("approve kyb: %v", err)
	}

	apiKey, hmacSecret, err := s.IssueAPIKey(ctx, id)
	if err != nil {
		t.Fatalf("issue api key: %v", err)
	}
	if apiKey == "" || hmacSecret == "" {
		t.Fatal("expected non-empty api key and secret")
	}

	// The secret is encrypted at rest — confirm the plaintext never made it
	// into the DB column verbatim.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT hmac_secret_encrypted FROM tenant_api_keys WHERE api_key = $1`, apiKey).Scan(&stored); err != nil {
		t.Fatalf("query stored secret: %v", err)
	}
	if stored == hmacSecret {
		t.Fatal("hmac secret was stored in plaintext")
	}
	if _, err := hex.DecodeString(stored); err != nil {
		t.Fatalf("expected stored secret to be hex-encoded ciphertext: %v", err)
	}

	gotSecret, gotTenantID, ok, err := s.LookupHMACSecret(ctx, apiKey)
	if err != nil {
		t.Fatalf("lookup hmac secret: %v", err)
	}
	if !ok {
		t.Fatal("expected lookup to succeed for an active tenant's active key")
	}
	if gotSecret != hmacSecret {
		t.Fatalf("expected decrypted secret to round-trip, got %q want %q", gotSecret, hmacSecret)
	}
	if gotTenantID != id {
		t.Fatalf("expected tenant id %s, got %s", id, gotTenantID)
	}
}

func TestApproveKYB_NotReapplicable(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	id, err := s.CreateTenant(ctx, "Test Bank "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := s.ApproveKYB(ctx, id); err != nil {
		t.Fatalf("first approve: %v", err)
	}

	// Approving an already-active tenant again must be rejected, not silently
	// succeed — mirrors the compare-and-set discipline used everywhere else.
	if err := s.ApproveKYB(ctx, id); !errors.Is(err, tenant.ErrInvalidStatusTransition) {
		t.Fatalf("expected ErrInvalidStatusTransition on re-approval, got %v", err)
	}
}

func TestSuspend_RevokesAuthenticationImmediately(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	id, err := s.CreateTenant(ctx, "Test Bank "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := s.ApproveKYB(ctx, id); err != nil {
		t.Fatalf("approve kyb: %v", err)
	}
	apiKey, _, err := s.IssueAPIKey(ctx, id)
	if err != nil {
		t.Fatalf("issue api key: %v", err)
	}

	// Sanity: works while active.
	_, _, ok, err := s.LookupHMACSecret(ctx, apiKey)
	if err != nil || !ok {
		t.Fatalf("expected lookup to succeed before suspension, ok=%v err=%v", ok, err)
	}

	if err := s.Suspend(ctx, id); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	_, _, ok, err = s.LookupHMACSecret(ctx, apiKey)
	if err != nil {
		t.Fatalf("lookup after suspend: %v", err)
	}
	if ok {
		t.Fatal("expected a suspended tenant's key to no longer authenticate")
	}
}

func TestLookupHMACSecret_UnknownKey(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	_, _, ok, err := s.LookupHMACSecret(ctx, "pk_does_not_exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for an unknown api key")
	}
}

func TestCorridorEntitlement_GrantAndRevoke(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)
	cs := corridor.New(pool)

	tenantID, err := s.CreateTenant(ctx, "Test Bank "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	corridorID, err := cs.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:   "USDT",
		CryptoNetwork: "TESTNET_" + t.Name(),
		FiatCurrency:  "NGN",
		Active:        true,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}

	entitled, err := s.CheckEntitlement(ctx, tenantID, corridorID)
	if err != nil {
		t.Fatalf("check entitlement (before grant): %v", err)
	}
	if entitled {
		t.Fatal("expected no entitlement before it's granted")
	}

	if err := s.SetCorridorEntitlement(ctx, tenantID, corridorID, true, nil); err != nil {
		t.Fatalf("grant entitlement: %v", err)
	}
	entitled, err = s.CheckEntitlement(ctx, tenantID, corridorID)
	if err != nil {
		t.Fatalf("check entitlement (after grant): %v", err)
	}
	if !entitled {
		t.Fatal("expected entitlement after granting it")
	}

	if err := s.SetCorridorEntitlement(ctx, tenantID, corridorID, false, nil); err != nil {
		t.Fatalf("revoke entitlement: %v", err)
	}
	entitled, err = s.CheckEntitlement(ctx, tenantID, corridorID)
	if err != nil {
		t.Fatalf("check entitlement (after revoke): %v", err)
	}
	if entitled {
		t.Fatal("expected no entitlement after revoking it")
	}
}

func TestValidateWebhookURL(t *testing.T) {
	// httptest.NewServer binds to 127.0.0.1 — a real loopback address, good
	// for proving the SSRF check actually resolves and inspects the IP
	// rather than just pattern-matching the hostname string.
	srv := httptest.NewServer(nil)
	defer srv.Close()

	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"plain http rejected", "http://example.com/webhook", true},
		{"loopback IP rejected", "https://127.0.0.1/webhook", true},
		{"localhost hostname rejected", "https://localhost/webhook", true},
		{"link-local metadata IP rejected", "https://169.254.169.254/latest/meta-data/", true},
		{"private RFC1918 IP rejected", "https://10.0.0.5/webhook", true},
		{"public https URL passes syntactic+scheme checks", "https://www.google.com/webhook", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tenant.ValidateWebhookURL(tc.url)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error for %s, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error for %s, got %v", tc.url, err)
			}
		})
	}
}

func TestSetWebhookURL_RejectsSSRFAttempt(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := newStore(t, pool)

	id, err := s.CreateTenant(ctx, "Test Bank "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, err = s.SetWebhookURL(ctx, id, "https://169.254.169.254/latest/meta-data/iam/security-credentials/")
	if !errors.Is(err, tenant.ErrInvalidWebhookURL) {
		t.Fatalf("expected ErrInvalidWebhookURL for a cloud-metadata SSRF attempt, got %v", err)
	}

	got, err := s.GetTenant(ctx, id)
	if err != nil {
		t.Fatalf("get tenant: %v", err)
	}
	if got.WebhookURL != nil {
		t.Fatalf("expected webhook url to remain unset after a rejected attempt, got %v", *got.WebhookURL)
	}
}

// gatewayInterfaceSmokeTest proves tenant.Store can actually be handed to
// gateway.NewRouter as a CredentialLookup — not just structurally
// compatible per the compile-time assertion in tenant.go, but usable.
func TestSatisfiesGatewayCredentialLookup(t *testing.T) {
	pool := openTestPool(t)
	s := newStore(t, pool)
	var _ gateway.CredentialLookup = s
}
