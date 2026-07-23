// Integration tests for Phase 5's orchestrator, verified against a live
// Postgres, following the same helper conventions as treasury_test.go/
// compliance_test.go (openTestPool, unique per-test corridor/currency).
package session_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/rate"
	"github.com/sirfi/payment-engine-v2/internal/session"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
	"github.com/sirfi/payment-engine-v2/internal/treasury"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

var testTenantEncryptionKey = []byte("01234567890123456789012345678901"[:32])

// testEnv is one fully-wired stack: a real tenant, entitled to a real
// corridor with a system rate set and a tenant-provided-wallet collection
// binding (avoids any external HTTP call or HD seed dependency), plus a
// session.Store wired to real corridor/compliance/rate/treasury stores.
type testEnv struct {
	pool       *db.Pool
	tenantID   uuid.UUID
	corridorID uuid.UUID
	session    *session.Store
	bus        *eventbus.Bus
}

// uniqueEVMAddress generates a fresh, well-formed (but not a real key's)
// EVM address per call.
func uniqueEVMAddress(t *testing.T) string {
	t.Helper()
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random address: %v", err)
	}
	return "0x" + hex.EncodeToString(b)
}

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

// fakeComplianceProvider lets tests exercise the registered-provider path
// without a real vendor integration — same test double compliance_test.go
// uses.
type fakeComplianceProvider struct {
	name     string
	decision compliance.Decision
}

func (f fakeComplianceProvider) Name() string { return f.name }
func (f fakeComplianceProvider) Screen(ctx context.Context, c compliance.Case) (compliance.Decision, error) {
	return f.decision, nil
}

// setupTestEnv builds one tenant + one corridor (unique per test), entitles
// the tenant, sets a system rate, and wires a tenant-provided-wallet
// collection binding — the one CollectionProvider that needs no HTTP call
// or loaded HD seed. If providerName is non-empty, a fake compliance
// provider deciding `decision` is registered and bound to the corridor at
// priority 1.
func setupTestEnv(t *testing.T, providerName string, decision compliance.Decision) *testEnv {
	t.Helper()
	pool := openTestPool(t)
	ctx := context.Background()

	tenantStore, err := tenant.New(pool, testTenantEncryptionKey)
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}
	tenantID, err := tenantStore.CreateTenant(ctx, "Session Test Tenant "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	fiatCurrency := "TST" + uuid.NewString()[:8]
	corridorStore := corridor.New(pool)
	corridorID, err := corridorStore.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:           "USDT",
		CryptoNetwork:         string(wallet.Ethereum),
		FiatCurrency:          fiatCurrency,
		Active:                true,
		TravelRuleWindow:      time.Hour,
		ComplianceHoldTimeout: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}
	if err := tenantStore.SetCorridorEntitlement(ctx, tenantID, corridorID, true, nil); err != nil {
		t.Fatalf("set corridor entitlement: %v", err)
	}

	rateStore := rate.New(pool, rate.Config{})
	if err := rateStore.SetSystemRate(ctx, fiatCurrency, decimal.NewFromInt(1000), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}

	treasuryStore := treasury.New(pool, corridorStore, treasury.Config{})
	// A unique address per test — treasury enforces a partial unique index
	// on (address) WHERE status='reserved', so reusing one literal address
	// across tests would collide as soon as more than one test reserves it.
	if err := treasuryStore.RegisterTenantCustomWallet(ctx, tenantID, wallet.Ethereum, uniqueEVMAddress(t), ""); err != nil {
		t.Fatalf("register tenant custom wallet: %v", err)
	}
	if _, err := corridorStore.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeCollection, "tenant_provided_wallet", 1, true, nil); err != nil {
		t.Fatalf("upsert collection provider binding: %v", err)
	}

	registry := compliance.NewRegistry()
	if providerName != "" {
		registry.Register(fakeComplianceProvider{name: providerName, decision: decision})
		if _, err := corridorStore.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeCompliance, providerName, 1, true, nil); err != nil {
			t.Fatalf("upsert compliance provider binding: %v", err)
		}
	}
	complianceStore := compliance.New(pool, registry)

	bus := eventbus.New(pool, 50)
	treasuryStore.SetEventBus(bus)
	sessionStore := session.New(pool, corridorStore, complianceStore, rateStore, treasuryStore, tenantStore, bus)
	sessionStore.RegisterEventHandlers()

	return &testEnv{pool: pool, tenantID: tenantID, corridorID: corridorID, session: sessionStore, bus: bus}
}

func (e *testEnv) createSession(t *testing.T, fiatAmount decimal.Decimal) *session.Session {
	t.Helper()
	sess, err := e.session.CreateSession(context.Background(), e.tenantID, "USDT", string(wallet.Ethereum), e.fiatCurrency(t), fiatAmount)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

// fiatCurrency re-derives the corridor's fiat currency from the DB rather
// than storing it separately on testEnv, keeping setupTestEnv's return
// shape minimal.
func (e *testEnv) fiatCurrency(t *testing.T) string {
	t.Helper()
	var currency string
	err := e.pool.QueryRow(context.Background(), `SELECT fiat_currency FROM corridors WHERE id = $1`, e.corridorID).Scan(&currency)
	if err != nil {
		t.Fatalf("lookup corridor fiat currency: %v", err)
	}
	return currency
}

func TestCreateSession_ApprovedReachesPendingWithDepositInstructions(t *testing.T) {
	env := setupTestEnv(t, "always-approve", compliance.Decision{Approved: true, Reason: "ok"})

	sess := env.createSession(t, decimal.NewFromInt(100))
	if sess.Status != session.StatusPending {
		t.Fatalf("expected pending, got %s", sess.Status)
	}
	if sess.RateLockID == nil {
		t.Fatal("expected rate_lock_id to be set")
	}
	if sess.DepositReservationID == nil {
		t.Fatal("expected deposit_reservation_id to be set")
	}
	if sess.ComplianceCaseID != nil {
		t.Fatal("expected no compliance_case_id on the approved path")
	}

	// session.created must have been published in the same transaction as
	// the pending transition — the first real Publish call on this
	// request path.
	var count int
	err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE event_type = 'session.created' AND aggregate_id = $1`,
		sess.ID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one session.created event, got %d", count)
	}
}

func TestCreateSession_ComplianceRejectionReachesRejected(t *testing.T) {
	env := setupTestEnv(t, "always-reject", compliance.Decision{Approved: false, Reason: "sanctions hit"})

	sess := env.createSession(t, decimal.NewFromInt(100))
	if sess.Status != session.StatusRejected {
		t.Fatalf("expected rejected, got %s", sess.Status)
	}
	if sess.RateLockID != nil || sess.DepositReservationID != nil {
		t.Fatal("expected no rate lock or deposit reservation on the rejected path")
	}
}

func TestCreateSession_NoComplianceProviderReachesHold(t *testing.T) {
	env := setupTestEnv(t, "", compliance.Decision{})

	sess := env.createSession(t, decimal.NewFromInt(100))
	if sess.Status != session.StatusComplianceHold {
		t.Fatalf("expected compliance_hold, got %s", sess.Status)
	}
	if sess.ComplianceCaseID == nil {
		t.Fatal("expected compliance_case_id to be set on the hold path")
	}
}

func TestCreateSession_NotEntitledCorridor(t *testing.T) {
	env := setupTestEnv(t, "always-approve", compliance.Decision{Approved: true, Reason: "ok"})

	// A second, un-entitled tenant against the same corridor.
	tenantStore, err := tenant.New(env.pool, testTenantEncryptionKey)
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}
	otherTenantID, err := tenantStore.CreateTenant(context.Background(), "Unentitled Tenant "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	_, err = env.session.CreateSession(context.Background(), otherTenantID, "USDT", string(wallet.Ethereum), env.fiatCurrency(t), decimal.NewFromInt(100))
	if err != session.ErrNotEntitled {
		t.Fatalf("expected ErrNotEntitled, got %v", err)
	}
}

func TestCreateSession_UnsupportedCorridor(t *testing.T) {
	env := setupTestEnv(t, "always-approve", compliance.Decision{Approved: true, Reason: "ok"})

	_, err := env.session.CreateSession(context.Background(), env.tenantID, "BTC", "bitcoin", "NOSUCHCURRENCY", decimal.NewFromInt(100))
	if err != session.ErrCorridorNotSupported {
		t.Fatalf("expected ErrCorridorNotSupported, got %v", err)
	}
}

func TestDepositEvents_DriveSessionThroughDetectedAndConfirmed(t *testing.T) {
	env := setupTestEnv(t, "always-approve", compliance.Decision{Approved: true, Reason: "ok"})
	sess := env.createSession(t, decimal.NewFromInt(100))
	if sess.Status != session.StatusPending {
		t.Fatalf("expected pending before any deposit event, got %s", sess.Status)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.bus.Run(runCtx, 50*time.Millisecond)

	publishReservationEvent(t, env, "treasury.deposit_detected", *sess.DepositReservationID)
	waitForStatus(t, env, sess.ID, session.StatusDepositDetected)

	publishReservationEvent(t, env, "treasury.deposit_confirmed", *sess.DepositReservationID)
	waitForStatus(t, env, sess.ID, session.StatusDepositConfirmed)

	assertOutboxEvent(t, env, "session.deposit_detected", sess.ID)
	assertOutboxEvent(t, env, "session.deposit_confirmed", sess.ID)
}

func TestDepositEvents_OutOfOrderConfirmedIsANoOp(t *testing.T) {
	env := setupTestEnv(t, "always-approve", compliance.Decision{Approved: true, Reason: "ok"})
	sess := env.createSession(t, decimal.NewFromInt(100))

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.bus.Run(runCtx, 50*time.Millisecond)

	// deposit_confirmed with no prior deposit_detected: the CAS predicate
	// (status = 'deposit_detected') never matches, so this must be a safe
	// no-op, not an error that blocks the dispatcher — same discipline
	// eventbus.Handler's doc comment requires for redelivered/out-of-order
	// events.
	publishReservationEvent(t, env, "treasury.deposit_confirmed", *sess.DepositReservationID)

	// Give the dispatcher a moment, then assert status is unchanged.
	time.Sleep(300 * time.Millisecond)
	got, err := env.session.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != session.StatusPending {
		t.Fatalf("expected status to remain pending, got %s", got.Status)
	}
}

// publishReservationEvent publishes a treasury deposit event keyed on
// reservationID directly via the bus, mirroring exactly what
// treasury.recordDepositTransition publishes — this isolates session's own
// subscribed-handler behavior from treasury's publishing logic, which is
// covered separately in internal/treasury's own tests.
func publishReservationEvent(t *testing.T, env *testEnv, eventType string, reservationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	payload, _ := json.Marshal(map[string]string{"tx_reference": "tst-" + uuid.NewString()})
	if err := env.bus.Publish(ctx, tx, eventbus.Event{
		EventType:     eventType,
		AggregateType: "treasury_deposit",
		AggregateID:   reservationID,
		Payload:       payload,
	}); err != nil {
		t.Fatalf("publish %s: %v", eventType, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func waitForStatus(t *testing.T, env *testEnv, sessionID uuid.UUID, want session.Status) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := env.session.GetSession(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if got.Status == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("session %s did not reach status %s within timeout", sessionID, want)
}

func assertOutboxEvent(t *testing.T, env *testEnv, eventType string, aggregateID uuid.UUID) {
	t.Helper()
	var count int
	err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox_events WHERE event_type = $1 AND aggregate_id = $2`,
		eventType, aggregateID,
	).Scan(&count)
	if err != nil {
		t.Fatalf("query outbox for %s: %v", eventType, err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one %s event for %s, got %d", eventType, aggregateID, count)
	}
}
