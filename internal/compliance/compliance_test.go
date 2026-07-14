package compliance_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
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
