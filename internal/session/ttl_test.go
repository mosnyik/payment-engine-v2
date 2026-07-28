package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/session"
)

// backdateSessionCreatedAt directly rewrites a session's created_at so TTL
// tests don't have to wait out the real 30-minute clock.
func backdateSessionCreatedAt(t *testing.T, env *testEnv, sessionID uuid.UUID, age time.Duration) {
	t.Helper()
	_, err := env.pool.Exec(context.Background(),
		`UPDATE sessions SET created_at = $2 WHERE id = $1`,
		sessionID, time.Now().Add(-age),
	)
	if err != nil {
		t.Fatalf("backdate session created_at: %v", err)
	}
}

// runTTLSweep runs one burst of session.TTLJob against a short poll
// interval, giving it enough time for at least one tick, then stops it.
func runTTLSweep(t *testing.T, env *testEnv) {
	t.Helper()
	job := session.NewTTLJob(env.session, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	job.Run(ctx)
}

func TestTTLJob_ExpiresPendingSessionPastTTL(t *testing.T) {
	env := setupTestEnv(t, "always-approve", compliance.Decision{Approved: true, Reason: "ok"})
	sess := env.createSession(t, decimal.NewFromInt(100))
	if sess.Status != session.StatusPending {
		t.Fatalf("expected pending, got %s", sess.Status)
	}

	backdateSessionCreatedAt(t, env, sess.ID, session.SessionTTL+time.Minute)
	runTTLSweep(t, env)

	got, err := env.session.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != session.StatusExpired {
		t.Fatalf("expected expired, got %s", got.Status)
	}
}

func TestTTLJob_DoesNotExpireInFlightDepositButMarksSLABreach(t *testing.T) {
	env := setupTestEnv(t, "always-approve", compliance.Decision{Approved: true, Reason: "ok"})
	sess := env.createSession(t, decimal.NewFromInt(100))

	// Move it to deposit_detected before backdating — a deposit already in
	// flight must never be force-expired, per ARCHITECTURE.md §8 rule 1,
	// even though it's well past the 30-minute mark.
	runCtx, cancel := context.WithCancel(context.Background())
	publishReservationEvent(t, env, "treasury.deposit_detected", *sess.DepositReservationID)
	go env.bus.Run(runCtx, 20*time.Millisecond)
	waitForStatus(t, env, sess.ID, session.StatusDepositDetected)
	cancel()

	backdateSessionCreatedAt(t, env, sess.ID, session.SessionTTL+time.Minute)
	runTTLSweep(t, env)

	got, err := env.session.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != session.StatusDepositDetected {
		t.Fatalf("expected status to remain deposit_detected (never force-expired with a deposit in flight), got %s", got.Status)
	}
	if got.SLABreachedAt == nil {
		t.Fatal("expected sla_breached_at to be set once the 30-minute mark passes, even though status is untouched")
	}
	assertOutboxEvent(t, env, "session.sla_breached", sess.ID)

	// A second sweep must not re-publish — sla_breached_at IS NULL in
	// markSLABreaches' WHERE clause already excludes this session the second
	// time around, same redelivery-safety reasoning as every other
	// publish-on-transition path in this package.
	runTTLSweep(t, env)
	assertOutboxEvent(t, env, "session.sla_breached", sess.ID)
}

func TestTTLJob_ExpiresTimedOutComplianceHoldWithoutClosingTheCase(t *testing.T) {
	env := setupTestEnv(t, "", compliance.Decision{})
	sess := env.createSession(t, decimal.NewFromInt(100))
	if sess.Status != session.StatusComplianceHold {
		t.Fatalf("expected compliance_hold, got %s", sess.Status)
	}

	// Backdate the case's hold_expires_at into the past instead of waiting
	// out the corridor's real (24h default) hold timeout.
	_, err := env.pool.Exec(context.Background(),
		`UPDATE compliance_cases SET hold_expires_at = $2 WHERE id = $1`,
		sess.ComplianceCaseID, time.Now().Add(-time.Minute),
	)
	if err != nil {
		t.Fatalf("backdate hold_expires_at: %v", err)
	}

	runTTLSweep(t, env)

	got, err := env.session.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != session.StatusExpired {
		t.Fatalf("expected expired, got %s", got.Status)
	}

	var caseStatus string
	err = env.pool.QueryRow(context.Background(), `SELECT status FROM compliance_cases WHERE id = $1`, sess.ComplianceCaseID).Scan(&caseStatus)
	if err != nil {
		t.Fatalf("query case status: %v", err)
	}
	if caseStatus != string(compliance.StatusHold) {
		t.Fatalf("expected the compliance case to remain open (hold) after session expiry — session expiry must never silently close it — got %s", caseStatus)
	}
}
