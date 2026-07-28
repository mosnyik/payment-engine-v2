// Internal test package (not notification_test) — tests use a fake
// TenantWebhookLookup pointed at a local httptest.Server, same convention
// treasury/tenant_notify_test.go establishes for the identical reason:
// tenant.SetWebhookURL's real SSRF validation correctly rejects loopback
// addresses (including a local test server's), so exercising delivery
// mechanics against a real tenant.Store here would be untestable by design,
// not a gap.
package notification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

var testTenantEncryptionKey = []byte("01234567890123456789012345678901"[:32])

// TestMain truncates this package's own table once before the suite runs —
// needed for the identical reason settlement_test.go's TestMain gives:
// DispatchWorker's (and here, runDispatchOnce's) claim is a genuinely
// global "oldest pending_dispatch row" query, so a stale row left behind by
// an interrupted previous run would otherwise get claimed ahead of an
// unrelated later test's own row.
func TestMain(m *testing.M) {
	_ = godotenv.Load("../../.env")
	if url := os.Getenv("DATABASE_URL"); url != "" {
		if pool, err := db.Open(context.Background(), url); err == nil {
			_, _ = pool.Exec(context.Background(), `TRUNCATE notification_deliveries`)
			pool.Close()
		}
	}
	os.Exit(m.Run())
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

// fakeTenantWebhookLookup is a test double satisfying TenantWebhookLookup —
// same convention treasury's fakeTenantWebhookLookup establishes, for the
// SSRF reason the package doc comment above explains.
type fakeTenantWebhookLookup struct {
	url    string
	secret string
	ok     bool
}

func (f *fakeTenantWebhookLookup) WebhookConfig(ctx context.Context, tenantID uuid.UUID) (string, string, bool, error) {
	return f.url, f.secret, f.ok, nil
}

// testEnv wires a real Store against a live Postgres, a real bus, and a
// fake webhook destination. A real tenant row still needs to exist (the FK
// on notification_deliveries.tenant_id), independent of where the fake
// lookup actually points deliveries.
type testEnv struct {
	pool     *db.Pool
	store    *Store
	bus      *eventbus.Bus
	tenantID uuid.UUID
	cancel   context.CancelFunc
	done     chan struct{}
}

func setupTestEnv(t *testing.T, lookup TenantWebhookLookup, cfg Config) *testEnv {
	t.Helper()
	pool := openTestPool(t)
	ctx := context.Background()

	tenantStore, err := tenant.New(pool, testTenantEncryptionKey)
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}
	tenantID, err := tenantStore.CreateTenant(ctx, "Notification Test Tenant "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	bus := eventbus.New(pool, 50)
	store := New(pool, lookup, cfg)
	store.SetEventBus(bus)
	store.RegisterEventHandlers()

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bus.Run(runCtx, 25*time.Millisecond)
	}()

	return &testEnv{pool: pool, store: store, bus: bus, tenantID: tenantID, cancel: cancel, done: done}
}

// Cleanup blocks until this env's bus.Run goroutine has actually exited —
// cancelling the context alone only signals it to stop on its next select
// iteration (up to one poll tick later). Without waiting here, a dying
// goroutine from one test can still be polling the same shared
// outbox_events/notification_deliveries tables when the next test's
// setupTestEnv starts a fresh one, racing to claim events across two
// different Store instances (found the hard way: it silently misattributes
// deliveries to the wrong test's fake tenant lookup).
func (e *testEnv) Cleanup() {
	e.cancel()
	<-e.done
}

// publish fabricates a domain event exactly as session/settlement/compliance
// publish it — a bare eventbus.Event with tenant_id in the payload, the
// shape every real publisher now guarantees (this phase's payload backfill).
func (e *testEnv) publish(t *testing.T, eventType string, extraPayload map[string]string) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	payloadMap := map[string]string{"tenant_id": e.tenantID.String()}
	for k, v := range extraPayload {
		payloadMap[k] = v
	}
	payload, _ := json.Marshal(payloadMap)
	if err := e.bus.Publish(ctx, tx, eventbus.Event{
		EventType:     eventType,
		AggregateType: "session",
		AggregateID:   uuid.New(),
		Payload:       payload,
	}); err != nil {
		t.Fatalf("publish %s: %v", eventType, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// waitForDeliveryCount polls this test's own tenant only — TestMain's
// truncate handles cross-run pollution, but scoping by tenant_id here too
// means one test's un-drained row (if any) can never be mistaken for
// another's within the same run, regardless of test order.
func (e *testEnv) waitForDeliveryCount(t *testing.T, channel Channel, want int) []Delivery {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var rows []Delivery
	for time.Now().Before(deadline) {
		dbRows, err := e.pool.Query(context.Background(),
			`SELECT `+deliveryColumns+` FROM notification_deliveries
			 WHERE tenant_id = $1 AND channel = $2 AND status = 'pending_dispatch'`,
			e.tenantID, string(channel),
		)
		if err != nil {
			t.Fatalf("query pending deliveries: %v", err)
		}
		rows = rows[:0]
		for dbRows.Next() {
			d, err := scanDelivery(dbRows)
			if err != nil {
				dbRows.Close()
				t.Fatalf("scan delivery: %v", err)
			}
			rows = append(rows, *d)
		}
		dbRows.Close()
		if len(rows) == want {
			return rows
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected %d pending %s deliveries for this test's tenant, got %d", want, channel, len(rows))
	return nil
}

// runDispatchOnce claims and processes exactly one pending_dispatch
// delivery synchronously — deterministic, unlike racing a ticker, same
// convention settlement_test.go's runDispatchOnce establishes.
func (e *testEnv) runDispatchOnce(t *testing.T) *Delivery {
	t.Helper()
	claimed, err := e.store.claimNextPendingDispatch(context.Background())
	if err != nil {
		t.Fatalf("claim next pending dispatch: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected a pending_dispatch delivery to claim, found none")
	}
	if err := e.store.processDelivery(context.Background(), claimed); err != nil {
		t.Fatalf("process delivery: %v", err)
	}
	got, err := e.store.GetDelivery(context.Background(), claimed.ID)
	if err != nil {
		t.Fatalf("reload delivery: %v", err)
	}
	return got
}

func TestWebhookDelivery_SignsAndDelivers(t *testing.T) {
	var receivedBody []byte
	var receivedSignature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		receivedSignature = r.Header.Get(webhookSignatureHeader)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	const secret = "test-notification-webhook-secret"
	env := setupTestEnv(t, &fakeTenantWebhookLookup{url: server.URL, secret: secret, ok: true}, Config{})
	defer env.Cleanup()

	env.publish(t, "session.created", map[string]string{"corridor_id": uuid.New().String()})
	rows := env.waitForDeliveryCount(t, ChannelWebhook, 1)

	got := env.runDispatchOnce(t)
	if got.Status != StatusDelivered {
		t.Fatalf("expected delivered, got %s (last_error=%v)", got.Status, got.LastError)
	}
	if got.ID != rows[0].ID {
		t.Fatalf("dispatched a different delivery than expected")
	}

	if receivedSignature == "" {
		t.Fatal("expected a signature header to be sent")
	}
	if want := ComputeWebhookSignature(secret, receivedBody); receivedSignature != want {
		t.Fatalf("signature mismatch: got %s, want %s", receivedSignature, want)
	}
	var decoded struct {
		EventType string `json:"event_type"`
		TenantID  string `json:"tenant_id"`
	}
	if err := json.Unmarshal(receivedBody, &decoded); err != nil {
		t.Fatalf("decode received body: %v", err)
	}
	if decoded.EventType != "session.created" || decoded.TenantID != env.tenantID.String() {
		t.Fatalf("unexpected body: %+v", decoded)
	}
}

func TestWebhookDelivery_NoWebhookConfigured_NoDeliveryRow(t *testing.T) {
	env := setupTestEnv(t, &fakeTenantWebhookLookup{ok: false}, Config{})
	defer env.Cleanup()

	env.publish(t, "session.created", nil)

	// A negative wait: give the handler time to run, then assert nothing
	// was ever inserted — not just that nothing is currently pending.
	time.Sleep(200 * time.Millisecond)
	var count int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notification_deliveries WHERE tenant_id = $1`, env.tenantID,
	).Scan(&count); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero deliveries for a tenant with no webhook configured, got %d", count)
	}
}

func TestEmailDelivery_DisabledProvider_DeadLettersImmediately(t *testing.T) {
	env := setupTestEnv(t, &fakeTenantWebhookLookup{ok: false}, Config{OpsAlertEmail: "ops@sirfi.test"})
	defer env.Cleanup()

	env.publish(t, "settlement.failed", map[string]string{"session_id": uuid.New().String()})
	env.waitForDeliveryCount(t, ChannelEmail, 1)

	got := env.runDispatchOnce(t)
	if got.Channel != ChannelEmail {
		t.Fatalf("expected the email delivery to be claimed, got channel %s", got.Channel)
	}
	if got.Status != StatusDeadLetter {
		t.Fatalf("expected dead_letter (email provider disabled by default), got %s", got.Status)
	}
	if got.LastError == nil || *got.LastError == "" {
		t.Fatal("expected last_error to explain the dead-letter")
	}
}

func TestComplianceHoldCreated_RoutesToEmailOnlyNotWebhook(t *testing.T) {
	const secret = "unused"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("compliance.hold_created must never deliver a tenant webhook")
	}))
	defer server.Close()

	env := setupTestEnv(t, &fakeTenantWebhookLookup{url: server.URL, secret: secret, ok: true}, Config{OpsAlertEmail: "ops@sirfi.test"})
	defer env.Cleanup()

	env.publish(t, "compliance.hold_created", map[string]string{"case_type": "kyb", "reference_type": "tenant", "reference_id": env.tenantID.String()})
	env.waitForDeliveryCount(t, ChannelEmail, 1)

	var webhookCount int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notification_deliveries WHERE tenant_id = $1 AND event_type = 'compliance.hold_created' AND channel = 'webhook'`,
		env.tenantID,
	).Scan(&webhookCount); err != nil {
		t.Fatalf("count webhook deliveries: %v", err)
	}
	if webhookCount != 0 {
		t.Fatalf("expected zero webhook deliveries for compliance.hold_created, got %d", webhookCount)
	}

	// Drain the email row this test created — leaving it pending_dispatch
	// would let a later test's runDispatchOnce (a genuinely global claim)
	// pick it up instead of that test's own row.
	env.runDispatchOnce(t)
}

func TestWebhookDelivery_FailureBacksOffThenDeadLetters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	env := setupTestEnv(t, &fakeTenantWebhookLookup{url: server.URL, secret: "s", ok: true}, Config{})
	defer env.Cleanup()

	env.publish(t, "session.created", nil)
	env.waitForDeliveryCount(t, ChannelWebhook, 1)

	got := env.runDispatchOnce(t)
	if got.Status != StatusPendingDispatch {
		t.Fatalf("expected pending_dispatch after a single failure (backoff, not dead-letter), got %s", got.Status)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("expected attempt_count 1, got %d", got.AttemptCount)
	}
	wantNext := time.Now().Add(backoffSteps[0])
	if got.NextAttemptAt.Before(wantNext.Add(-10*time.Second)) || got.NextAttemptAt.After(wantNext.Add(10*time.Second)) {
		t.Fatalf("expected next_attempt_at ~%s (first backoff step), got %s", wantNext, got.NextAttemptAt)
	}

	// Fast-forward through the remaining backoff steps deterministically —
	// same "cheat time in the DB rather than sleep for real" approach this
	// test needs, since the real schedule spans hours.
	ctx := context.Background()
	for i := 1; i < len(backoffSteps); i++ {
		if _, err := env.pool.Exec(ctx, `UPDATE notification_deliveries SET next_attempt_at = now() WHERE id = $1`, got.ID); err != nil {
			t.Fatalf("force next_attempt_at: %v", err)
		}
		got = env.runDispatchOnce(t)
		if got.Status != StatusPendingDispatch {
			t.Fatalf("attempt %d: expected still pending_dispatch, got %s", i+1, got.Status)
		}
	}

	// One more failure past the schedule's end must dead-letter.
	if _, err := env.pool.Exec(ctx, `UPDATE notification_deliveries SET next_attempt_at = now() WHERE id = $1`, got.ID); err != nil {
		t.Fatalf("force next_attempt_at: %v", err)
	}
	got = env.runDispatchOnce(t)
	if got.Status != StatusDeadLetter {
		t.Fatalf("expected dead_letter after exhausting the backoff schedule, got %s", got.Status)
	}
	if got.AttemptCount != len(backoffSteps)+1 {
		t.Fatalf("expected attempt_count %d, got %d", len(backoffSteps)+1, got.AttemptCount)
	}
}

func TestRedeliveredEvent_ProducesOneDeliveryRow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	env := setupTestEnv(t, &fakeTenantWebhookLookup{url: server.URL, secret: "s", ok: true}, Config{})
	defer env.Cleanup()

	aggregateID := uuid.New()
	publishSame := func() {
		ctx := context.Background()
		tx, err := env.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		payload, _ := json.Marshal(map[string]string{"tenant_id": env.tenantID.String()})
		if err := env.bus.Publish(ctx, tx, eventbus.Event{
			EventType: "session.created", AggregateType: "session", AggregateID: aggregateID, Payload: payload,
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	publishSame()
	publishSame()

	env.waitForDeliveryCount(t, ChannelWebhook, 1)

	var count int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notification_deliveries WHERE aggregate_id = $1 AND event_type = 'session.created' AND channel = 'webhook'`,
		aggregateID,
	).Scan(&count); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one delivery row for a redelivered event, got %d", count)
	}

	// Drain it — same reasoning as TestComplianceHoldCreated's drain.
	env.runDispatchOnce(t)
}

func TestRetryDelivery_ResetsDeadLetteredRowForAnotherAttempt(t *testing.T) {
	env := setupTestEnv(t, &fakeTenantWebhookLookup{ok: false}, Config{OpsAlertEmail: "ops@sirfi.test"})
	defer env.Cleanup()

	env.publish(t, "settlement.failed", map[string]string{"session_id": uuid.New().String()})
	env.waitForDeliveryCount(t, ChannelEmail, 1)

	got := env.runDispatchOnce(t)
	if got.Status != StatusDeadLetter {
		t.Fatalf("expected dead_letter, got %s", got.Status)
	}

	retried, err := env.store.RetryDelivery(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("retry delivery: %v", err)
	}
	if retried.Status != StatusPendingDispatch {
		t.Fatalf("expected pending_dispatch after retry, got %s", retried.Status)
	}
	if retried.AttemptCount != 0 {
		t.Fatalf("expected attempt_count reset to 0, got %d", retried.AttemptCount)
	}

	// Drain it — every test in this file must leave zero pending_dispatch
	// rows behind, full stop, same discipline settlement_test.go's own
	// tests follow. runDispatchOnce's claim is genuinely global (by design —
	// production must drain the whole queue), so a stray pending row left
	// behind here would get claimed ahead of an unrelated future test's own
	// row, exactly the failure mode this whole test file is written to avoid.
	env.runDispatchOnce(t)
}

// TestSLABreached_RoutesToEmailOnlyNotWebhook covers Phase 8's SLA-breach
// alerting (internal/session/ttl.go's markSLABreaches): the tenant already
// sees the session's real pipeline stage untouched, so this is ops-only,
// same treatment as compliance.hold_created.
func TestSLABreached_RoutesToEmailOnlyNotWebhook(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("session.sla_breached must never deliver a tenant webhook")
	}))
	defer server.Close()

	env := setupTestEnv(t, &fakeTenantWebhookLookup{url: server.URL, secret: "unused", ok: true}, Config{OpsAlertEmail: "ops@sirfi.test"})
	defer env.Cleanup()

	env.publish(t, "session.sla_breached", nil)
	env.waitForDeliveryCount(t, ChannelEmail, 1)

	var webhookCount int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM notification_deliveries WHERE tenant_id = $1 AND event_type = 'session.sla_breached' AND channel = 'webhook'`,
		env.tenantID,
	).Scan(&webhookCount); err != nil {
		t.Fatalf("count webhook deliveries: %v", err)
	}
	if webhookCount != 0 {
		t.Fatalf("expected zero webhook deliveries for session.sla_breached, got %d", webhookCount)
	}

	env.runDispatchOnce(t)
}

// TestLedgerDriftDetected_RoutesToEmailEvenWithoutTenantID covers Phase 8's
// reconciliation job (internal/ledger/reconcile.go): unlike every other
// event this package handles, a platform/omnibus ledger account genuinely
// has no owning tenant — tenant_id must come through as NULL, not error out
// or silently get dropped.
func TestLedgerDriftDetected_RoutesToEmailEvenWithoutTenantID(t *testing.T) {
	env := setupTestEnv(t, &fakeTenantWebhookLookup{ok: false}, Config{OpsAlertEmail: "ops@sirfi.test"})
	defer env.Cleanup()

	accountID := uuid.New()
	ctx := context.Background()
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"tenant_id":    nil,
		"account_id":   accountID.String(),
		"drift_amount": "12.5",
	})
	if err := env.bus.Publish(ctx, tx, eventbus.Event{
		EventType: "ledger.drift_detected", AggregateType: "ledger_account", AggregateID: accountID, Payload: payload,
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var deliveryID uuid.UUID
	var tenantID *uuid.UUID
	for time.Now().Before(deadline) {
		err := env.pool.QueryRow(context.Background(),
			`SELECT id, tenant_id FROM notification_deliveries WHERE aggregate_id = $1 AND event_type = 'ledger.drift_detected' AND channel = 'email'`,
			accountID,
		).Scan(&deliveryID, &tenantID)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if deliveryID == uuid.Nil {
		t.Fatal("expected an email delivery row for ledger.drift_detected")
	}
	if tenantID != nil {
		t.Fatalf("expected a NULL tenant_id for a platform-account drift, got %s", *tenantID)
	}

	env.runDispatchOnce(t)
}
