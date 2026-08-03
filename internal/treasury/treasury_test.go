// Internal test package (not treasury_test) — tests need to fabricate
// Store.providers directly with fakes so they never make a real HTTP call
// to Busha's nonexistent endpoint, without adding a test-only public seam
// to the production API.
package treasury

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

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/tenant"
)

// testTenantEncryptionKey is a throwaway key for tenant.New in tests —
// treasury's tests only need a real tenants row to satisfy
// treasury_address_reservations' tenant_id FK, never a tenant's actual
// secrets.
var testTenantEncryptionKey = []byte("01234567890123456789012345678901"[:32])

// createTestTenant inserts a real tenants row so tests can use its id as
// a valid tenant_id — treasury_address_reservations has a real FK to
// tenants(id) (migration 000016), so a fabricated uuid.New() no longer
// satisfies it the way it did before tenant-scoping.
func createTestTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	ts, err := tenant.New(pool, testTenantEncryptionKey)
	if err != nil {
		t.Fatalf("new tenant store: %v", err)
	}
	id, err := ts.CreateTenant(context.Background(), "Test Tenant "+uuid.NewString())
	if err != nil {
		t.Fatalf("create test tenant: %v", err)
	}
	return id
}

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

// uniqueAsset keeps each test's corridor from colliding with data left over
// from a previous run against the same live database.
func uniqueAsset(t *testing.T) string {
	t.Helper()
	return "TST_" + t.Name()
}

type providerBinding struct {
	name     string
	priority int
}

// setupCorridor creates (or updates) a test corridor and wires the given
// collection-provider bindings onto it, priority-ordered as given.
func setupCorridor(t *testing.T, pool *db.Pool, bindings ...providerBinding) (*corridor.Store, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	cs := corridor.New(pool)

	corridorID, err := cs.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:           uniqueAsset(t),
		CryptoNetwork:         "TESTNET",
		FiatCurrency:          "TSTUSD",
		Active:                true,
		TravelRuleWindow:      time.Hour,
		ComplianceHoldTimeout: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}

	for _, b := range bindings {
		if _, err := cs.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeCollection, b.name, b.priority, true, nil); err != nil {
			t.Fatalf("upsert provider binding %q: %v", b.name, err)
		}
	}

	return cs, corridorID
}

// fakeCollectionProvider is a test double — the real Busha adapter makes an
// HTTP call to a TODO endpoint that doesn't exist yet, so failover and
// webhook-handling logic is tested against this instead.
type fakeCollectionProvider struct {
	name    string
	enabled bool
	custody CustodyType
	address ProviderAddress
	err     error
}

func (f *fakeCollectionProvider) Name() string             { return f.name }
func (f *fakeCollectionProvider) IsEnabled() bool          { return f.enabled }
func (f *fakeCollectionProvider) CustodyType() CustodyType { return f.custody }
func (f *fakeCollectionProvider) ReserveAddress(_ context.Context, _ uuid.UUID, _, _ string) (ProviderAddress, error) {
	if f.err != nil {
		return ProviderAddress{}, f.err
	}
	return f.address, nil
}

func newTestStore(pool *db.Pool, corridorStore *corridor.Store, providers ...*fakeCollectionProvider) *Store {
	m := make(map[string]CollectionProvider, len(providers))
	for _, p := range providers {
		m[p.name] = p
	}
	return &Store{
		pool:               pool,
		corridorStore:      corridorStore,
		providers:          m,
		bushaWebhookSecret: "test-webhook-secret",
	}
}

func TestReserveAddress_PicksHighestPriorityProvider(t *testing.T) {
	pool := openTestPool(t)
	addr := "addr-primary-" + uniqueAsset(t)
	primary := &fakeCollectionProvider{
		name: "primary", enabled: true, custody: CustodyTypePartner,
		address: ProviderAddress{Address: addr, ProviderReference: "ref-primary"},
	}
	backup := &fakeCollectionProvider{
		name: "backup", enabled: true, custody: CustodyTypePartner,
		address: ProviderAddress{Address: "addr-backup-" + uniqueAsset(t), ProviderReference: "ref-backup"},
	}
	cs, corridorID := setupCorridor(t, pool,
		providerBinding{name: "primary", priority: 1},
		providerBinding{name: "backup", priority: 2},
	)
	s := newTestStore(pool, cs, primary, backup)

	r, err := s.ReserveAddress(context.Background(), createTestTenant(t, pool), corridorID)
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	releaseReservationOnCleanup(t, s, r.ID)
	if r.ProviderName != "primary" || r.Address != addr {
		t.Fatalf("expected primary provider's address, got provider=%s address=%s", r.ProviderName, r.Address)
	}
}

func TestReserveAddress_FailsOverOnError(t *testing.T) {
	pool := openTestPool(t)
	failing := &fakeCollectionProvider{name: "failing", enabled: true, custody: CustodyTypePartner, err: errors.New("boom")}
	working := &fakeCollectionProvider{
		name: "working", enabled: true, custody: CustodyTypePartner,
		address: ProviderAddress{Address: "addr-working-" + uniqueAsset(t), ProviderReference: "ref-working"},
	}
	cs, corridorID := setupCorridor(t, pool,
		providerBinding{name: "failing", priority: 1},
		providerBinding{name: "working", priority: 2},
	)
	s := newTestStore(pool, cs, failing, working)

	r, err := s.ReserveAddress(context.Background(), createTestTenant(t, pool), corridorID)
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	releaseReservationOnCleanup(t, s, r.ID)
	if r.ProviderName != "working" {
		t.Fatalf("expected failover to working provider, got %s", r.ProviderName)
	}
}

func TestReserveAddress_NoActiveProvider(t *testing.T) {
	pool := openTestPool(t)
	cs, corridorID := setupCorridor(t, pool) // no bindings at all
	s := newTestStore(pool, cs)

	_, err := s.ReserveAddress(context.Background(), createTestTenant(t, pool), corridorID)
	if !errors.Is(err, ErrNoProviderAvailable) {
		t.Fatalf("expected ErrNoProviderAvailable, got %v", err)
	}
}

func webhookBody(t *testing.T, eventType, referenceID, txReference string, amount decimal.Decimal) []byte {
	t.Helper()
	body, err := json.Marshal(bushaWebhookPayload{
		EventType:         DepositEventType(eventType),
		ProviderReference: referenceID,
		TxReference:       txReference,
		Amount:            amount,
	})
	if err != nil {
		t.Fatalf("marshal webhook body: %v", err)
	}
	return body
}

func reserveTestAddress(t *testing.T, s *Store, corridorID uuid.UUID, providerRef string) *AddressReservation {
	t.Helper()
	fake := &fakeCollectionProvider{
		name: "webhooktest", enabled: true, custody: CustodyTypePartner,
		address: ProviderAddress{Address: "addr-" + providerRef, ProviderReference: providerRef},
	}
	s.providers["webhooktest"] = fake
	ctx := context.Background()
	cs := s.corridorStore
	if _, err := cs.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeCollection, "webhooktest", 1, true, nil); err != nil {
		t.Fatalf("upsert provider binding: %v", err)
	}
	r, err := s.ReserveAddress(ctx, createTestTenant(t, s.pool), corridorID)
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	releaseReservationOnCleanup(t, s, r.ID)
	return r
}

// releaseReservationOnCleanup releases a reservation once a test finishes.
// This package's test addresses are deterministic per test name (via
// uniqueAsset), not per run, so leaving a reservation 'reserved' would
// collide with idx_treasury_reservations_open_address (migration 000011)
// on the next run against the same live database.
func releaseReservationOnCleanup(t *testing.T, s *Store, reservationID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(),
			`UPDATE treasury_address_reservations SET status = 'released', released_at = now() WHERE id = $1`, reservationID)
	})
}

func TestHandleDepositWebhook_DetectedThenConfirmed(t *testing.T) {
	pool := openTestPool(t)
	cs, corridorID := setupCorridor(t, pool)
	s := newTestStore(pool, cs)
	r := reserveTestAddress(t, s, corridorID, "ref-"+uniqueAsset(t))
	ctx := context.Background()

	detectedBody := webhookBody(t, string(DepositEventDetected), r.ProviderReference, "tx-1", decimal.NewFromInt(1))
	sig := ComputeWebhookSignature(s.bushaWebhookSecret, detectedBody)
	if err := s.HandleDepositWebhook(ctx, detectedBody, sig); err != nil {
		t.Fatalf("handle detected webhook: %v", err)
	}
	d, err := s.GetDeposit(ctx, r.ID, "tx-1")
	if err != nil {
		t.Fatalf("get deposit: %v", err)
	}
	if d.Status != "detected" {
		t.Fatalf("expected status detected, got %s", d.Status)
	}

	confirmedBody := webhookBody(t, string(DepositEventConfirmed), r.ProviderReference, "tx-1", decimal.NewFromInt(1))
	sig = ComputeWebhookSignature(s.bushaWebhookSecret, confirmedBody)
	if err := s.HandleDepositWebhook(ctx, confirmedBody, sig); err != nil {
		t.Fatalf("handle confirmed webhook: %v", err)
	}
	d, err = s.GetDeposit(ctx, r.ID, "tx-1")
	if err != nil {
		t.Fatalf("get deposit after confirm: %v", err)
	}
	if d.Status != "confirmed" {
		t.Fatalf("expected status confirmed, got %s", d.Status)
	}
	if d.ConfirmedAt == nil {
		t.Fatalf("expected confirmed_at to be set")
	}
}

func TestHandleDepositWebhook_ConfirmedBeforeDetected(t *testing.T) {
	pool := openTestPool(t)
	cs, corridorID := setupCorridor(t, pool)
	s := newTestStore(pool, cs)
	r := reserveTestAddress(t, s, corridorID, "ref-"+uniqueAsset(t))
	ctx := context.Background()

	body := webhookBody(t, string(DepositEventConfirmed), r.ProviderReference, "tx-1", decimal.NewFromInt(1))
	sig := ComputeWebhookSignature(s.bushaWebhookSecret, body)
	if err := s.HandleDepositWebhook(ctx, body, sig); err != nil {
		t.Fatalf("handle confirmed-first webhook: %v", err)
	}
	d, err := s.GetDeposit(ctx, r.ID, "tx-1")
	if err != nil {
		t.Fatalf("get deposit: %v", err)
	}
	if d.Status != "confirmed" {
		t.Fatalf("expected status confirmed even without a prior detected webhook, got %s", d.Status)
	}
}

func TestHandleDepositWebhook_ReplayIsNoOp(t *testing.T) {
	pool := openTestPool(t)
	cs, corridorID := setupCorridor(t, pool)
	s := newTestStore(pool, cs)
	r := reserveTestAddress(t, s, corridorID, "ref-"+uniqueAsset(t))
	ctx := context.Background()

	body := webhookBody(t, string(DepositEventDetected), r.ProviderReference, "tx-1", decimal.NewFromInt(1))
	sig := ComputeWebhookSignature(s.bushaWebhookSecret, body)
	if err := s.HandleDepositWebhook(ctx, body, sig); err != nil {
		t.Fatalf("first webhook: %v", err)
	}
	if err := s.HandleDepositWebhook(ctx, body, sig); err != nil {
		t.Fatalf("replayed webhook should be a no-op, got error: %v", err)
	}
	d, err := s.GetDeposit(ctx, r.ID, "tx-1")
	if err != nil {
		t.Fatalf("get deposit: %v", err)
	}
	if d.Status != "detected" {
		t.Fatalf("expected replay to leave status unchanged at detected, got %s", d.Status)
	}
}

func TestHandleDepositWebhook_PublishesEventsOnlyOnRealTransitions(t *testing.T) {
	pool := openTestPool(t)
	cs, corridorID := setupCorridor(t, pool)
	s := newTestStore(pool, cs)
	bus := eventbus.New(pool, 50)
	s.SetEventBus(bus)
	r := reserveTestAddress(t, s, corridorID, "ref-"+uniqueAsset(t))
	ctx := context.Background()

	countEvents := func(eventType string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM outbox_events WHERE event_type = $1 AND aggregate_id = $2`,
			eventType, r.ID,
		).Scan(&n); err != nil {
			t.Fatalf("count %s events: %v", eventType, err)
		}
		return n
	}

	detectedBody := webhookBody(t, string(DepositEventDetected), r.ProviderReference, "tx-1", decimal.NewFromInt(1))
	sig := ComputeWebhookSignature(s.bushaWebhookSecret, detectedBody)
	if err := s.HandleDepositWebhook(ctx, detectedBody, sig); err != nil {
		t.Fatalf("handle detected webhook: %v", err)
	}
	if n := countEvents("treasury.deposit_detected"); n != 1 {
		t.Fatalf("expected exactly 1 treasury.deposit_detected event, got %d", n)
	}

	// A replayed 'detected' webhook is a no-op at the DB level (ON CONFLICT
	// DO NOTHING) and must not publish a second event for it.
	if err := s.HandleDepositWebhook(ctx, detectedBody, sig); err != nil {
		t.Fatalf("replayed detected webhook: %v", err)
	}
	if n := countEvents("treasury.deposit_detected"); n != 1 {
		t.Fatalf("expected replay not to publish a duplicate event, still got %d", n)
	}

	confirmedBody := webhookBody(t, string(DepositEventConfirmed), r.ProviderReference, "tx-1", decimal.NewFromInt(1))
	sig = ComputeWebhookSignature(s.bushaWebhookSecret, confirmedBody)
	if err := s.HandleDepositWebhook(ctx, confirmedBody, sig); err != nil {
		t.Fatalf("handle confirmed webhook: %v", err)
	}
	if n := countEvents("treasury.deposit_confirmed"); n != 1 {
		t.Fatalf("expected exactly 1 treasury.deposit_confirmed event, got %d", n)
	}

	// A replayed 'confirmed' webhook hits the CAS's WHERE status='detected'
	// with no matching row (already confirmed) — must not publish again.
	if err := s.HandleDepositWebhook(ctx, confirmedBody, sig); err != nil {
		t.Fatalf("replayed confirmed webhook: %v", err)
	}
	if n := countEvents("treasury.deposit_confirmed"); n != 1 {
		t.Fatalf("expected replay not to publish a duplicate event, still got %d", n)
	}
}

// TestHandleDepositWebhook_NoEventBusIsSafe confirms every existing test in
// this file (none of which call SetEventBus) is unaffected — a Store with
// no bus configured must keep working exactly as before.
func TestHandleDepositWebhook_NoEventBusIsSafe(t *testing.T) {
	pool := openTestPool(t)
	cs, corridorID := setupCorridor(t, pool)
	s := newTestStore(pool, cs)
	r := reserveTestAddress(t, s, corridorID, "ref-"+uniqueAsset(t))
	ctx := context.Background()

	body := webhookBody(t, string(DepositEventDetected), r.ProviderReference, "tx-1", decimal.NewFromInt(1))
	sig := ComputeWebhookSignature(s.bushaWebhookSecret, body)
	if err := s.HandleDepositWebhook(ctx, body, sig); err != nil {
		t.Fatalf("handle detected webhook with no event bus configured: %v", err)
	}
}

func TestHandleDepositWebhook_InvalidSignature(t *testing.T) {
	pool := openTestPool(t)
	cs, corridorID := setupCorridor(t, pool)
	s := newTestStore(pool, cs)
	r := reserveTestAddress(t, s, corridorID, "ref-"+uniqueAsset(t))
	ctx := context.Background()

	body := webhookBody(t, string(DepositEventDetected), r.ProviderReference, "tx-1", decimal.NewFromInt(1))
	err := s.HandleDepositWebhook(ctx, body, "not-the-real-signature")
	if !errors.Is(err, ErrInvalidWebhookSignature) {
		t.Fatalf("expected ErrInvalidWebhookSignature, got %v", err)
	}
}

func TestCustodyBalance_RoundTrip(t *testing.T) {
	pool := openTestPool(t)
	s := newTestStore(pool, corridor.New(pool))
	ctx := context.Background()
	asset := uniqueAsset(t)

	asOf := time.Now().Truncate(time.Second)
	if err := s.RecordCustodyBalance(ctx, "busha", asset, decimal.NewFromInt(100), asOf); err != nil {
		t.Fatalf("record custody balance: %v", err)
	}
	got, err := s.GetCustodyBalance(ctx, "busha", asset)
	if err != nil {
		t.Fatalf("get custody balance: %v", err)
	}
	if !got.Balance.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("expected balance 100, got %s", got.Balance)
	}

	// Re-record with a different value — must update in place, not error
	// or create a second row (same upsert convention as rate.SetSystemRate).
	if err := s.RecordCustodyBalance(ctx, "busha", asset, decimal.NewFromInt(150), asOf.Add(time.Minute)); err != nil {
		t.Fatalf("re-record custody balance: %v", err)
	}
	got2, err := s.GetCustodyBalance(ctx, "busha", asset)
	if err != nil {
		t.Fatalf("get custody balance after re-record: %v", err)
	}
	if !got2.Balance.Equal(decimal.NewFromInt(150)) {
		t.Fatalf("expected updated balance 150, got %s", got2.Balance)
	}
}
