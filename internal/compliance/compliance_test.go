package compliance_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
	}

	pool, err := db.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeProvider lets tests exercise the registered-provider path without a
// real vendor integration.
type fakeProvider struct {
	name     string
	decision compliance.Decision
	err      error
}

func (f fakeProvider) Name() string { return f.name }
func (f fakeProvider) Screen(ctx context.Context, c compliance.Case) (compliance.Decision, error) {
	return f.decision, f.err
}

// mustCreateAdmin gives ResolveHold tests a real admin_users row to satisfy
// the resolved_by foreign key.
func mustCreateAdmin(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	store := adminauth.New(pool, 0)
	id, err := store.CreateAdmin(context.Background(), "compliance-test-"+uuid.NewString()+"@sirfi.test", "irrelevant-password")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	return id
}

var testTenantEncryptionKey = []byte("01234567890123456789012345678901"[:32])

// createTestTenantAndCorridor gives ScreenSession tests real tenants/
// corridors rows to satisfy sessions' foreign keys.
func createTestTenantAndCorridor(t *testing.T, pool *db.Pool) (tenantID, corridorID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	ts, err := tenant.New(pool, testTenantEncryptionKey)
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}
	tenantID, err = ts.CreateTenant(ctx, "Test Tenant "+uuid.NewString())
	if err != nil {
		t.Fatalf("create test tenant: %v", err)
	}

	cs := corridor.New(pool)
	corridorID, err = cs.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:   "TST_" + uuid.NewString(),
		CryptoNetwork: "TESTNET",
		FiatCurrency:  "TSTUSD",
		Active:        true,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}
	return tenantID, corridorID
}

// insertRawSession inserts a sessions row directly (bypassing
// internal/session, which itself depends on this package — importing it
// here would work but adds an unnecessary cross-package test dependency)
// so ScreenSession's rolling Travel Rule volume query has real prior
// volume to sum.
func insertRawSession(t *testing.T, pool *db.Pool, tenantID, corridorID uuid.UUID, fiatAmount decimal.Decimal, status string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO sessions (tenant_id, corridor_id, status, fiat_currency, fiat_amount, crypto_asset, crypto_network)
		 VALUES ($1, $2, $3, 'TSTUSD', $4, 'TST', 'TESTNET')`,
		tenantID, corridorID, status, fiatAmount,
	)
	if err != nil {
		t.Fatalf("insert raw session: %v", err)
	}
}

func TestScreenSession_ApprovedNoThreshold(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := compliance.NewRegistry()
	reg.Register(fakeProvider{name: "test-provider", decision: compliance.Decision{Approved: true, Reason: "ok"}})
	s := compliance.New(pool, reg)

	tenantID, _ := createTestTenantAndCorridor(t, pool)
	c, err := s.ScreenSession(ctx, uuid.New(), tenantID, decimal.NewFromInt(100), nil, time.Hour, 24*time.Hour, "test-provider")
	if err != nil {
		t.Fatalf("screen session: %v", err)
	}
	if c.Status != compliance.StatusApproved {
		t.Fatalf("expected approved, got %s", c.Status)
	}
	if c.ReferenceType != "session" {
		t.Fatalf("expected reference_type session, got %s", c.ReferenceType)
	}
	if c.HoldExpiresAt != nil {
		t.Fatal("expected no hold_expires_at for an approved case")
	}
}

func TestScreenSession_NoProviderFallsIntoHoldWithExpiry(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := compliance.New(pool, compliance.NewRegistry())

	tenantID, _ := createTestTenantAndCorridor(t, pool)
	before := time.Now()
	c, err := s.ScreenSession(ctx, uuid.New(), tenantID, decimal.NewFromInt(100), nil, time.Hour, 2*time.Hour, "")
	if err != nil {
		t.Fatalf("screen session: %v", err)
	}
	if c.Status != compliance.StatusHold {
		t.Fatalf("expected hold, got %s", c.Status)
	}
	if c.HoldExpiresAt == nil {
		t.Fatal("expected hold_expires_at to be set for a session hold (unlike KYB holds)")
	}
	// Loose bound (rather than exact equality) to absorb DB round-trip
	// timestamp precision/rounding, not just call latency.
	expectedMin := before.Add(2 * time.Hour).Add(-time.Second)
	expectedMax := before.Add(2 * time.Hour).Add(time.Minute)
	if c.HoldExpiresAt.Before(expectedMin) || c.HoldExpiresAt.After(expectedMax) {
		t.Fatalf("expected hold_expires_at within [%s, %s], got %s", expectedMin, expectedMax, c.HoldExpiresAt)
	}
}

func TestScreenSession_ForcedHoldViaTravelRuleThreshold(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	// A provider that would approve everything — proving the Travel Rule
	// hold below overrides it rather than deferring to the provider.
	reg := compliance.NewRegistry()
	reg.Register(fakeProvider{name: "always-approve", decision: compliance.Decision{Approved: true, Reason: "ok"}})
	s := compliance.New(pool, reg)

	tenantID, corridorID := createTestTenantAndCorridor(t, pool)
	insertRawSession(t, pool, tenantID, corridorID, decimal.NewFromInt(600), "pending")

	threshold := decimal.NewFromInt(1000)
	// 600 (prior, within window) + 500 (this session) = 1100 >= 1000 threshold.
	c, err := s.ScreenSession(ctx, uuid.New(), tenantID, decimal.NewFromInt(500), &threshold, time.Hour, 24*time.Hour, "always-approve")
	if err != nil {
		t.Fatalf("screen session: %v", err)
	}
	if c.Status != compliance.StatusHold {
		t.Fatalf("expected hold forced by travel rule threshold, got %s", c.Status)
	}
	if c.DecisionReason == nil || *c.DecisionReason != "travel rule rolling volume threshold exceeded" {
		t.Fatalf("expected travel rule decision reason, got %v", c.DecisionReason)
	}
	if c.ProviderName != nil {
		t.Fatalf("expected no provider_name when forced to hold by travel rule, got %v", c.ProviderName)
	}
}

func TestScreenSession_TravelRuleIgnoresOutOfWindowVolumeAndRejectedSessions(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := compliance.NewRegistry()
	reg.Register(fakeProvider{name: "always-approve-2", decision: compliance.Decision{Approved: true, Reason: "ok"}})
	s := compliance.New(pool, reg)

	tenantID, corridorID := createTestTenantAndCorridor(t, pool)
	// Outside the 1-hour window and a rejected session both must not count
	// toward rolling volume.
	insertRawSession(t, pool, tenantID, corridorID, decimal.NewFromInt(900), "rejected")

	threshold := decimal.NewFromInt(1000)
	c, err := s.ScreenSession(ctx, uuid.New(), tenantID, decimal.NewFromInt(500), &threshold, time.Hour, 24*time.Hour, "always-approve-2")
	if err != nil {
		t.Fatalf("screen session: %v", err)
	}
	if c.Status != compliance.StatusApproved {
		t.Fatalf("expected approved (rejected prior session shouldn't count toward volume), got %s", c.Status)
	}
}

func TestScreenTenant_NoProviderFallsIntoHold(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := compliance.New(pool, compliance.NewRegistry())

	tenantID := uuid.New()
	c, err := s.ScreenTenant(ctx, tenantID, json.RawMessage(`{"company":"Test Bank"}`), "")
	if err != nil {
		t.Fatalf("screen tenant: %v", err)
	}
	if c.Status != compliance.StatusHold {
		t.Fatalf("expected status hold with no provider, got %s", c.Status)
	}
	if c.HoldExpiresAt != nil {
		t.Fatal("expected no hold_expires_at for a KYB case — it has no session-like TTL")
	}
	if c.ReferenceType != "tenant" || c.ReferenceID != tenantID {
		t.Fatalf("expected reference tenant/%s, got %s/%s", tenantID, c.ReferenceType, c.ReferenceID)
	}
}

func TestScreenTenant_RegisteredProviderApproves(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := compliance.NewRegistry()
	reg.Register(fakeProvider{name: "test-provider", decision: compliance.Decision{Approved: true, Reason: "docs verified"}})
	s := compliance.New(pool, reg)

	tenantID := uuid.New()
	c, err := s.ScreenTenant(ctx, tenantID, json.RawMessage(`{}`), "test-provider")
	if err != nil {
		t.Fatalf("screen tenant: %v", err)
	}
	if c.Status != compliance.StatusApproved {
		t.Fatalf("expected status approved, got %s", c.Status)
	}
	if c.ProviderName == nil || *c.ProviderName != "test-provider" {
		t.Fatalf("expected provider_name test-provider, got %v", c.ProviderName)
	}
	if c.ResolvedAt == nil {
		t.Fatal("expected resolved_at to be set for an auto-decided case")
	}
}

func TestScreenTenant_RegisteredProviderRejects(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := compliance.NewRegistry()
	reg.Register(fakeProvider{name: "test-provider-reject", decision: compliance.Decision{Approved: false, Reason: "sanctions hit"}})
	s := compliance.New(pool, reg)

	c, err := s.ScreenTenant(ctx, uuid.New(), json.RawMessage(`{}`), "test-provider-reject")
	if err != nil {
		t.Fatalf("screen tenant: %v", err)
	}
	if c.Status != compliance.StatusRejected {
		t.Fatalf("expected status rejected, got %s", c.Status)
	}
	if c.DecisionReason == nil || *c.DecisionReason != "sanctions hit" {
		t.Fatalf("expected decision reason 'sanctions hit', got %v", c.DecisionReason)
	}
}

func TestScreenTenant_UnknownProviderNameErrors(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := compliance.New(pool, compliance.NewRegistry())

	_, err := s.ScreenTenant(ctx, uuid.New(), json.RawMessage(`{}`), "does-not-exist")
	if !errors.Is(err, compliance.ErrProviderNotFound) {
		t.Fatalf("expected ErrProviderNotFound, got %v", err)
	}
}

func TestGetLatestCase(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := compliance.New(pool, compliance.NewRegistry())

	tenantID := uuid.New()
	_, err := s.GetLatestCase(ctx, "tenant", tenantID, compliance.CaseTypeKYB)
	if !errors.Is(err, compliance.ErrNotFound) {
		t.Fatalf("expected ErrNotFound before any case exists, got %v", err)
	}

	created, err := s.ScreenTenant(ctx, tenantID, json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatalf("screen tenant: %v", err)
	}

	latest, err := s.GetLatestCase(ctx, "tenant", tenantID, compliance.CaseTypeKYB)
	if err != nil {
		t.Fatalf("get latest case: %v", err)
	}
	if latest.ID != created.ID {
		t.Fatalf("expected latest case %s, got %s", created.ID, latest.ID)
	}
}

func TestListHolds_AndResolveHold(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := compliance.New(pool, compliance.NewRegistry())
	adminID := mustCreateAdmin(t, pool)

	tenantID := uuid.New()
	held, err := s.ScreenTenant(ctx, tenantID, json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatalf("screen tenant: %v", err)
	}
	if held.Status != compliance.StatusHold {
		t.Fatalf("expected hold, got %s", held.Status)
	}

	holds, err := s.ListHolds(ctx, compliance.CaseTypeKYB)
	if err != nil {
		t.Fatalf("list holds: %v", err)
	}
	found := false
	for _, c := range holds {
		if c.ID == held.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the newly-created hold to appear in ListHolds")
	}

	resolved, err := s.ResolveHold(ctx, held.ID, adminID, true, "manually verified documents")
	if err != nil {
		t.Fatalf("resolve hold: %v", err)
	}
	if resolved.Status != compliance.StatusApproved {
		t.Fatalf("expected approved after resolving, got %s", resolved.Status)
	}
	if resolved.ResolvedBy == nil || *resolved.ResolvedBy != adminID {
		t.Fatalf("expected resolved_by %s, got %v", adminID, resolved.ResolvedBy)
	}
	if resolved.ProviderName == nil || *resolved.ProviderName != "manual" {
		t.Fatalf("expected provider_name 'manual', got %v", resolved.ProviderName)
	}

	// Resolving an already-resolved hold must be rejected, not silently
	// re-applied — same compare-and-set discipline as everywhere else.
	_, err = s.ResolveHold(ctx, held.ID, adminID, false, "changed my mind")
	if !errors.Is(err, compliance.ErrHoldAlreadyResolved) {
		t.Fatalf("expected ErrHoldAlreadyResolved, got %v", err)
	}
}

func TestResolveHold_ConcurrentResolutionOnlyOneWins(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := compliance.New(pool, compliance.NewRegistry())
	admin1 := mustCreateAdmin(t, pool)
	admin2 := mustCreateAdmin(t, pool)

	held, err := s.ScreenTenant(ctx, uuid.New(), json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatalf("screen tenant: %v", err)
	}

	type result struct {
		err error
	}
	results := make(chan result, 2)
	go func() {
		_, err := s.ResolveHold(ctx, held.ID, admin1, true, "admin1 approved")
		results <- result{err}
	}()
	go func() {
		_, err := s.ResolveHold(ctx, held.ID, admin2, false, "admin2 rejected")
		results <- result{err}
	}()

	successes, failures := 0, 0
	for range 2 {
		r := <-results
		if r.err == nil {
			successes++
		} else if errors.Is(r.err, compliance.ErrHoldAlreadyResolved) {
			failures++
		} else {
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("expected exactly one success and one ErrHoldAlreadyResolved, got %d successes and %d failures", successes, failures)
	}
}
