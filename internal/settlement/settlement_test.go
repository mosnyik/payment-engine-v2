// Internal test package (not settlement_test) — tests need to fabricate
// Store.providers directly with fakes so they never make a real HTTP call
// to a nonexistent TODO endpoint, same convention treasury_test.go
// establishes for the identical reason.
package settlement

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/rate"
	"github.com/sirfi/payment-engine-v2/internal/session"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
	"github.com/sirfi/payment-engine-v2/internal/treasury"
	"github.com/sirfi/payment-engine-v2/internal/treasury/wallet"
)

var testTenantEncryptionKey = []byte("01234567890123456789012345678901"[:32])

// TestMain truncates this package's own tables once before the suite runs.
// Needed because DispatchWorker's claim is a genuinely global "oldest
// pending_dispatch row" query (by design — production must drain the whole
// queue, not just one caller's row) — a stale row left behind by an
// interrupted previous run of this same suite would otherwise get claimed
// by an unrelated later test's runDispatchOnce call. Tenants/corridors/
// sessions are left alone (harmless to accumulate, each test uses a unique
// fiat currency/asset) — only the tables a stale pending_dispatch row could
// pollute are cleared.
func TestMain(m *testing.M) {
	_ = godotenv.Load("../../.env")
	if url := os.Getenv("DATABASE_URL"); url != "" {
		if pool, err := db.Open(context.Background(), url); err == nil {
			_, _ = pool.Exec(context.Background(), `TRUNCATE settlement_reversals, settlement_attempts, settlements`)
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

// fakeSettlementProvider is a test double — the real named adapters
// (cngn/flutterwave/...) all call a TODO endpoint that doesn't exist yet, so
// dispatch/retry/webhook logic is tested against this instead, same
// convention treasury_test.go's fakeCollectionProvider establishes.
type fakeSettlementProvider struct {
	name    string
	enabled bool
	outcome DispatchOutcome
	ref     string
	reason  string
	secret  string
	calls   int
}

func (f *fakeSettlementProvider) Name() string    { return f.name }
func (f *fakeSettlementProvider) IsEnabled() bool { return f.enabled }
func (f *fakeSettlementProvider) webhookSecret() string {
	if f.secret == "" {
		return "test-webhook-secret-" + f.name
	}
	return f.secret
}
func (f *fakeSettlementProvider) Dispatch(_ context.Context, _ PayoutRequest) (PayoutResult, error) {
	f.calls++
	return PayoutResult{Outcome: f.outcome, ProviderReference: f.ref, FailureReason: f.reason}, nil
}

// fakeComplianceProvider lets tests reach 'approved' without a real vendor
// integration — same test double session_test.go/compliance_test.go use.
type fakeComplianceProvider struct{}

func (fakeComplianceProvider) Name() string { return "always-approve" }
func (fakeComplianceProvider) Screen(_ context.Context, _ compliance.Case) (compliance.Decision, error) {
	return compliance.Decision{Approved: true, Reason: "ok"}, nil
}

// testEnv is one fully-wired stack: a real tenant (1% fee via corridor
// entitlement override) entitled to a real corridor with a system rate,
// tenant-provided-wallet collection binding, and always-approve compliance
// — plus session and settlement Stores sharing one live bus, mirroring
// production wiring (cmd/server/stores.go) closely enough that
// session.deposit_confirmed really does drive settlement's own subscriber.
type testEnv struct {
	pool       *db.Pool
	tenantID   uuid.UUID
	corridorID uuid.UUID
	fiat       string
	session    *session.Store
	settlement *Store
	bus        *eventbus.Bus
	cancel     context.CancelFunc
}

func setupTestEnv(t *testing.T, providers ...*fakeSettlementProvider) *testEnv {
	t.Helper()
	pool := openTestPool(t)
	ctx := context.Background()

	tenantStore, err := tenant.New(pool, testTenantEncryptionKey)
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}
	tenantID, err := tenantStore.CreateTenant(ctx, "Settlement Test Tenant "+uuid.NewString())
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	fiat := "TST" + uuid.NewString()[:8]
	corridorStore := corridor.New(pool)
	corridorID, err := corridorStore.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:           "USDT",
		CryptoNetwork:         string(wallet.Ethereum),
		FiatCurrency:          fiat,
		Active:                true,
		TravelRuleWindow:      time.Hour,
		ComplianceHoldTimeout: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}
	feeBps := 100 // 1%, matches ARCHITECTURE.md §6's worked example fee rate
	if err := tenantStore.SetCorridorEntitlement(ctx, tenantID, corridorID, true, &feeBps); err != nil {
		t.Fatalf("set corridor entitlement: %v", err)
	}

	rateStore := rate.New(pool, rate.Config{})
	// System rate 1000, 1% slippage buffer -> locked rate 990. USDT's asset
	// price is hardcoded to 1 (rate.GetAssetPrice), so 100 USDT locks to a
	// deterministic, exactly-assertable 99,000 fiat.
	if err := rateStore.SetSystemRate(ctx, fiat, decimal.NewFromInt(1000), decimal.Zero, decimal.Zero); err != nil {
		t.Fatalf("set system rate: %v", err)
	}

	treasuryStore := treasury.New(pool, corridorStore, treasury.Config{})
	if err := treasuryStore.RegisterTenantCustomWallet(ctx, tenantID, wallet.Ethereum, uniqueEVMAddress(t), ""); err != nil {
		t.Fatalf("register tenant custom wallet: %v", err)
	}
	if _, err := corridorStore.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeCollection, "tenant_provided_wallet", 1, true, nil); err != nil {
		t.Fatalf("upsert collection provider binding: %v", err)
	}

	registry := compliance.NewRegistry()
	registry.Register(fakeComplianceProvider{})
	if _, err := corridorStore.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeCompliance, "always-approve", 1, true, nil); err != nil {
		t.Fatalf("upsert compliance provider binding: %v", err)
	}
	complianceStore := compliance.New(pool, registry)

	for i, p := range providers {
		if _, err := corridorStore.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeSettlement, p.name, i+1, true, nil); err != nil {
			t.Fatalf("upsert settlement provider binding: %v", err)
		}
	}
	providerMap := make(map[string]SettlementProvider, len(providers))
	for _, p := range providers {
		providerMap[p.name] = p
	}

	bus := eventbus.New(pool, 50)
	treasuryStore.SetEventBus(bus)
	sessionStore := session.New(pool, corridorStore, complianceStore, rateStore, treasuryStore, tenantStore, bus)
	sessionStore.RegisterEventHandlers()

	settlementStore := &Store{
		pool:          pool,
		ledger:        ledger.New(pool),
		corridorStore: corridorStore,
		sessionStore:  sessionStore,
		treasuryStore: treasuryStore,
		rateStore:     rateStore,
		feeResolver:   tenantStore,
		providers:     providerMap,
	}
	settlementStore.SetEventBus(bus)
	settlementStore.RegisterEventHandlers()

	runCtx, cancel := context.WithCancel(context.Background())
	go bus.Run(runCtx, 25*time.Millisecond)

	return &testEnv{
		pool: pool, tenantID: tenantID, corridorID: corridorID, fiat: fiat,
		session: sessionStore, settlement: settlementStore, bus: bus, cancel: cancel,
	}
}

func (e *testEnv) Cleanup() { e.cancel() }

func uniqueEVMAddress(t *testing.T) string {
	t.Helper()
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random address: %v", err)
	}
	return "0x" + hex.EncodeToString(b)
}

// createDepositConfirmedSession drives a real session through screening ->
// pending -> deposit_detected -> deposit_confirmed via the same event path
// production uses, then inserts a matching confirmed treasury_deposits row
// (the amount DispatchWorker will actually sum) and waits for settlement's
// own subscriber to create the settlements row.
func (e *testEnv) createDepositConfirmedSession(t *testing.T, fiatAmount, cryptoAmount decimal.Decimal) *session.Session {
	t.Helper()
	ctx := context.Background()

	sess, err := e.session.CreateSession(ctx, e.tenantID, "USDT", string(wallet.Ethereum), e.fiat, fiatAmount, nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.Status != session.StatusPending {
		t.Fatalf("expected pending, got %s", sess.Status)
	}

	if _, err := e.pool.Exec(ctx,
		`INSERT INTO treasury_deposits (reservation_id, status, crypto_asset, amount, tx_reference, confirmed_at)
		 VALUES ($1, 'confirmed', 'USDT', $2, $3, now())`,
		*sess.DepositReservationID, cryptoAmount, "tst-"+uuid.NewString(),
	); err != nil {
		t.Fatalf("insert confirmed deposit: %v", err)
	}

	e.publishReservationEvent(t, "treasury.deposit_detected", *sess.DepositReservationID)
	e.waitForSessionStatus(t, sess.ID, session.StatusDepositDetected)
	e.publishReservationEvent(t, "treasury.deposit_confirmed", *sess.DepositReservationID)
	e.waitForSessionStatus(t, sess.ID, session.StatusDepositConfirmed)
	e.waitForSettlementRow(t, sess.ID)

	got, err := e.session.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	return got
}

func (e *testEnv) publishReservationEvent(t *testing.T, eventType string, reservationID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := e.bus.Publish(ctx, tx, eventbus.Event{
		EventType:     eventType,
		AggregateType: "treasury_deposit",
		AggregateID:   reservationID,
	}); err != nil {
		t.Fatalf("publish %s: %v", eventType, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func (e *testEnv) waitForSessionStatus(t *testing.T, sessionID uuid.UUID, want session.Status) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, err := e.session.GetSession(context.Background(), sessionID)
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

func (e *testEnv) waitForSettlementRow(t *testing.T, sessionID uuid.UUID) *Settlement {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, err := e.settlement.GetSettlementBySession(context.Background(), sessionID)
		if err == nil {
			return st
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("settlement row for session %s never appeared", sessionID)
	return nil
}

// runDispatchOnce claims and processes exactly one pending_dispatch
// settlement synchronously — deterministic, unlike racing a ticker.
func (e *testEnv) runDispatchOnce(t *testing.T) {
	t.Helper()
	claimed, err := e.settlement.claimNextPendingDispatch(context.Background())
	if err != nil {
		t.Fatalf("claim next pending dispatch: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected a pending_dispatch settlement to claim, found none")
	}
	if err := e.settlement.processSettlement(context.Background(), claimed); err != nil {
		t.Fatalf("process settlement: %v", err)
	}
}

func (e *testEnv) waitForSettlementStatus(t *testing.T, settlementID uuid.UUID, want Status) *Settlement {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, err := e.settlement.GetSettlement(context.Background(), settlementID)
		if err != nil {
			t.Fatalf("get settlement: %v", err)
		}
		if st.Status == want {
			return st
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("settlement %s did not reach status %s within timeout", settlementID, want)
	return nil
}

func (e *testEnv) balance(t *testing.T, l *ledger.Ledger, tenantID *uuid.UUID, accountType, assetCode, unitType string) decimal.Decimal {
	t.Helper()
	id, err := l.GetOrCreateAccount(context.Background(), tenantID, accountType, assetCode, unitType, accountType+":"+assetCode)
	if err != nil {
		t.Fatalf("get or create account %s:%s: %v", accountType, assetCode, err)
	}
	bal, err := l.GetBalance(context.Background(), id)
	if err != nil {
		t.Fatalf("get balance %s:%s: %v", accountType, assetCode, err)
	}
	return bal
}

func TestDispatch_HappyPath_SettlesAndPostsLedgerEntries(t *testing.T) {
	provider := &fakeSettlementProvider{name: "fakeprovider", enabled: true, outcome: OutcomeAccepted, ref: "ref-" + uuid.NewString()}
	env := setupTestEnv(t, provider)
	defer env.Cleanup()

	// treasury_in_transit/crypto_fx_clearing are platform-level accounts
	// keyed only on asset code ("USDT") — genuinely shared across every test
	// in this suite (and every prior run against this live DB), unlike the
	// fiat accounts below which are naturally isolated per test via
	// env.fiat's unique currency code. Assert deltas for the shared ones,
	// not absolute values.
	l := ledger.New(env.pool)
	inTransitBefore := env.balance(t, l, nil, "treasury_in_transit", "USDT", "crypto")
	cryptoClearingBefore := env.balance(t, l, nil, "crypto_fx_clearing", "USDT", "crypto")

	sess := env.createDepositConfirmedSession(t, decimal.NewFromInt(99000), decimal.NewFromInt(100))
	st := env.waitForSettlementRow(t, sess.ID)

	env.runDispatchOnce(t)
	st = env.waitForSettlementStatus(t, st.ID, StatusSettling)

	if !st.FiatValue.Equal(decimal.NewFromInt(99000)) {
		t.Fatalf("expected fiat_value 99000, got %s", st.FiatValue)
	}
	if !st.FeeAmount.Equal(decimal.NewFromInt(990)) {
		t.Fatalf("expected fee_amount 990, got %s", st.FeeAmount)
	}
	if !st.TenantPayableAmount.Equal(decimal.NewFromInt(98010)) {
		t.Fatalf("expected tenant_payable_amount 98010, got %s", st.TenantPayableAmount)
	}
	if provider.calls != 1 {
		t.Fatalf("expected exactly 1 dispatch call, got %d", provider.calls)
	}

	sessAfterSettling, err := env.session.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sessAfterSettling.Status != session.StatusSettling {
		t.Fatalf("expected session settling, got %s", sessAfterSettling.Status)
	}

	// Deliver the success webhook.
	if err := env.settlement.HandleSettlementWebhook(context.Background(), provider.name, []byte(`{"event_type":"payout.succeeded","reference_id":"`+provider.ref+`"}`),
		ComputeWebhookSignature(provider.webhookSecret(), []byte(`{"event_type":"payout.succeeded","reference_id":"`+provider.ref+`"}`))); err != nil {
		t.Fatalf("handle webhook: %v", err)
	}
	env.waitForSettlementStatus(t, st.ID, StatusSettled)
	env.waitForSessionStatus(t, sess.ID, session.StatusSettled)

	// The full ledger trail (ARCHITECTURE.md §6): deposit_confirmed,
	// fx_conversion, settlement_payout — asset accounts net to zero, fiat
	// accounts land exactly on the fee split.
	if bal := env.balance(t, l, nil, "treasury_in_transit", "USDT", "crypto"); !bal.Sub(inTransitBefore).Equal(decimal.NewFromInt(100)) {
		t.Fatalf("treasury_in_transit: expected +100 delta, got %s (before=%s after=%s)", bal.Sub(inTransitBefore), inTransitBefore, bal)
	}
	if bal := env.balance(t, l, nil, "crypto_fx_clearing", "USDT", "crypto"); !bal.Sub(cryptoClearingBefore).Equal(decimal.NewFromInt(-100)) {
		t.Fatalf("crypto_fx_clearing: expected -100 delta, got %s (before=%s after=%s)", bal.Sub(cryptoClearingBefore), cryptoClearingBefore, bal)
	}
	if bal := env.balance(t, l, nil, "fiat_fx_clearing", env.fiat, "fiat"); !bal.Equal(decimal.NewFromInt(99000)) {
		t.Fatalf("fiat_fx_clearing: expected 99000, got %s", bal)
	}
	if bal := env.balance(t, l, nil, "fee_revenue", env.fiat, "fiat"); !bal.Equal(decimal.NewFromInt(-990)) {
		t.Fatalf("fee_revenue: expected -990, got %s", bal)
	}
	if bal := env.balance(t, l, nil, "treasury_fiat_operating", env.fiat, "fiat"); !bal.Equal(decimal.NewFromInt(-98010)) {
		t.Fatalf("treasury_fiat_operating: expected -98010, got %s", bal)
	}
	tid := env.tenantID
	if bal := env.balance(t, l, &tid, "tenant_payable", env.fiat, "fiat"); !bal.IsZero() {
		t.Fatalf("tenant_payable: expected 0 (98010 credited by fx_conversion, 98010 debited by settlement_payout), got %s", bal)
	}

	// The reservation is released only at this terminal state (§8 rule 5).
	var reservationStatus string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT status FROM treasury_address_reservations WHERE id = $1`, *sessAfterSettling.DepositReservationID,
	).Scan(&reservationStatus); err != nil {
		t.Fatalf("query reservation status: %v", err)
	}
	if reservationStatus != "released" {
		t.Fatalf("expected reservation released, got %s", reservationStatus)
	}
}

func TestDispatch_Bucket1Failover_SettlesOnSecondProvider(t *testing.T) {
	failing := &fakeSettlementProvider{name: "failing", enabled: true, outcome: OutcomeRejectedRetryable, reason: "network error"}
	working := &fakeSettlementProvider{name: "working", enabled: true, outcome: OutcomeAccepted, ref: "ref-" + uuid.NewString()}
	env := setupTestEnv(t, failing, working)
	defer env.Cleanup()

	sess := env.createDepositConfirmedSession(t, decimal.NewFromInt(99000), decimal.NewFromInt(100))
	st := env.waitForSettlementRow(t, sess.ID)

	env.runDispatchOnce(t)
	st = env.waitForSettlementStatus(t, st.ID, StatusSettling)

	if failing.calls != 1 || working.calls != 1 {
		t.Fatalf("expected exactly one call to each provider, got failing=%d working=%d", failing.calls, working.calls)
	}
	if st.AttemptCount != 2 {
		t.Fatalf("expected attempt_count 2 after failover, got %d", st.AttemptCount)
	}

	var succeededProvider string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT provider_name FROM settlement_attempts WHERE settlement_id = $1 AND status = 'dispatched'`, st.ID,
	).Scan(&succeededProvider); err != nil {
		t.Fatalf("query dispatched attempt: %v", err)
	}
	if succeededProvider != "working" {
		t.Fatalf("expected the dispatched attempt to be on 'working', got %s", succeededProvider)
	}
}

func TestDispatch_Bucket4Terminal_SettlementFailedImmediately(t *testing.T) {
	provider := &fakeSettlementProvider{name: "terminal", enabled: true, outcome: OutcomeRejectedTerminal, reason: "invalid account"}
	env := setupTestEnv(t, provider)
	defer env.Cleanup()

	sess := env.createDepositConfirmedSession(t, decimal.NewFromInt(99000), decimal.NewFromInt(100))
	st := env.waitForSettlementRow(t, sess.ID)

	env.runDispatchOnce(t)
	st = env.waitForSettlementStatus(t, st.ID, StatusSettlementFailed)

	if provider.calls != 1 {
		t.Fatalf("expected exactly 1 dispatch call (no retry on bucket 4), got %d", provider.calls)
	}
	if st.OpsPagedAt == nil {
		t.Fatal("expected ops_paged_at to be set")
	}
	env.waitForSessionStatus(t, sess.ID, session.StatusSettlementFailed)

	// fx_conversion credited tenant_payable 98010 (liability recognized,
	// balance -98010 under this ledger's debits-minus-credits convention).
	// settlement_payout then optimistically debited it back to 0 before
	// calling the provider; the compensating reversal must put the
	// liability BACK to -98010 — the payout never actually went out, so the
	// tenant is still owed the money pending a future retry, not settled to
	// zero (ARCHITECTURE.md §6's claim-then-reverse pattern).
	l := ledger.New(env.pool)
	tid := env.tenantID
	if bal := env.balance(t, l, &tid, "tenant_payable", env.fiat, "fiat"); !bal.Equal(decimal.NewFromInt(-98010)) {
		t.Fatalf("tenant_payable: expected -98010 (liability restored) after compensating reversal, got %s", bal)
	}
}

func TestWebhook_InvalidSignatureRejected(t *testing.T) {
	provider := &fakeSettlementProvider{name: "sigtest", enabled: true, outcome: OutcomeAccepted}
	env := setupTestEnv(t, provider)
	defer env.Cleanup()

	err := env.settlement.HandleSettlementWebhook(context.Background(), provider.name, []byte(`{}`), "not-the-right-signature")
	if err != ErrInvalidWebhookSignature {
		t.Fatalf("expected ErrInvalidWebhookSignature, got %v", err)
	}
}

func TestWebhook_SucceededReplayIsNoOp(t *testing.T) {
	provider := &fakeSettlementProvider{name: "replaytest", enabled: true, outcome: OutcomeAccepted, ref: "ref-" + uuid.NewString()}
	env := setupTestEnv(t, provider)
	defer env.Cleanup()

	sess := env.createDepositConfirmedSession(t, decimal.NewFromInt(99000), decimal.NewFromInt(100))
	st := env.waitForSettlementRow(t, sess.ID)
	env.runDispatchOnce(t)
	env.waitForSettlementStatus(t, st.ID, StatusSettling)

	body := []byte(`{"event_type":"payout.succeeded","reference_id":"` + provider.ref + `"}`)
	sig := ComputeWebhookSignature(provider.webhookSecret(), body)

	if err := env.settlement.HandleSettlementWebhook(context.Background(), provider.name, body, sig); err != nil {
		t.Fatalf("first webhook delivery: %v", err)
	}
	env.waitForSettlementStatus(t, st.ID, StatusSettled)

	// A redelivered webhook must be a safe no-op, not an error and not a
	// second ledger post (ledger.Post's idempotency key would reject it
	// anyway, but the CAS on settlement_attempts should short-circuit
	// before ever trying).
	if err := env.settlement.HandleSettlementWebhook(context.Background(), provider.name, body, sig); err != nil {
		t.Fatalf("replayed webhook should be a no-op, got error: %v", err)
	}
}
