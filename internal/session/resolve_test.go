package session_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/session"
)

func mustCreateAdmin(t *testing.T, env *testEnv) uuid.UUID {
	t.Helper()
	store := adminauth.New(env.pool, 0)
	id, err := store.CreateAdmin(context.Background(), "session-test-"+uuid.NewString()+"@sirfi.test", "irrelevant-password")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	return id
}

func TestResolveComplianceHold_ApprovedReachesPending(t *testing.T) {
	env := setupTestEnv(t, "", compliance.Decision{})
	sess := env.createSession(t, decimal.NewFromInt(100))
	if sess.Status != session.StatusComplianceHold {
		t.Fatalf("expected compliance_hold, got %s", sess.Status)
	}
	adminID := mustCreateAdmin(t, env)

	resolved, err := env.session.ResolveComplianceHold(context.Background(), sess.ID, adminID, true, "manually verified")
	if err != nil {
		t.Fatalf("resolve compliance hold: %v", err)
	}
	if resolved.Status != session.StatusPending {
		t.Fatalf("expected pending, got %s", resolved.Status)
	}
	if resolved.RateLockID == nil || resolved.DepositReservationID == nil {
		t.Fatal("expected rate lock and deposit reservation to be set after approval")
	}
}

func TestResolveComplianceHold_RejectedReachesRejected(t *testing.T) {
	env := setupTestEnv(t, "", compliance.Decision{})
	sess := env.createSession(t, decimal.NewFromInt(100))
	adminID := mustCreateAdmin(t, env)

	resolved, err := env.session.ResolveComplianceHold(context.Background(), sess.ID, adminID, false, "confirmed sanctions hit")
	if err != nil {
		t.Fatalf("resolve compliance hold: %v", err)
	}
	if resolved.Status != session.StatusRejected {
		t.Fatalf("expected rejected, got %s", resolved.Status)
	}
}

func TestResolveComplianceHold_NotInHoldErrors(t *testing.T) {
	env := setupTestEnv(t, "always-approve", compliance.Decision{Approved: true, Reason: "ok"})
	sess := env.createSession(t, decimal.NewFromInt(100))
	if sess.Status != session.StatusPending {
		t.Fatalf("expected pending, got %s", sess.Status)
	}
	adminID := mustCreateAdmin(t, env)

	_, err := env.session.ResolveComplianceHold(context.Background(), sess.ID, adminID, true, "n/a")
	if err != session.ErrNotInComplianceHold {
		t.Fatalf("expected ErrNotInComplianceHold, got %v", err)
	}
}
