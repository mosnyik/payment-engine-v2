// Package settlement is Phase 6's payout pipeline: the ledger-claim-then-
// dispatch pattern (ARCHITECTURE.md §6) fired by session.deposit_confirmed,
// driving a session from deposit_confirmed through settling to settled (or
// settlement_failed/reversed) via signature-verified provider webhooks and a
// bounded, bucket-classified retry/failover policy (ARCHITECTURE.md §8).
//
// ledger.Post always opens its own transaction (never joins a caller's), so
// it can never be called from inside an eventbus.Handler — see that type's
// doc comment. That's why the session.deposit_confirmed subscriber
// (events.go) does nothing but claim a local row; DispatchWorker
// (dispatch.go), a ticker-driven background worker like session.TTLJob, does
// the real ledger posting and provider dispatch.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/rate"
	"github.com/sirfi/payment-engine-v2/internal/session"
	"github.com/sirfi/payment-engine-v2/internal/treasury"
)

type Status string

const (
	// StatusPendingDispatch is settlements-row-local, not one of session's
	// states — it exists purely so DispatchWorker has something to claim.
	StatusPendingDispatch  Status = "pending_dispatch"
	StatusDispatching      Status = "dispatching"
	StatusSettling         Status = "settling"
	StatusSettled          Status = "settled"
	StatusSettlementFailed Status = "settlement_failed"
	StatusReversed         Status = "reversed"
	StatusReversalResolved Status = "reversal_resolved"
)

// Retry policy constants (ARCHITECTURE.md §8's "Settlement retry policy") —
// compiled-in design decisions, not ops knobs, same convention as
// session.SessionTTL and rate.slippageBuffer.
const (
	MaxAutoRetryAttempts = 3
	ConfirmationTimeout  = 10 * time.Minute
	RetryBackoff         = 60 * time.Second
)

var (
	ErrNotFound          = errors.New("settlement: not found")
	ErrNoProviderAvailable = errors.New("settlement: no active, enabled settlement provider available for this corridor")
)

// Settlement is one session's settlement lifecycle record.
type Settlement struct {
	ID                     uuid.UUID
	SessionID              uuid.UUID
	TenantID               uuid.UUID
	CorridorID             uuid.UUID
	Status                 Status
	CryptoAsset            string
	CryptoAmount           decimal.Decimal
	FiatCurrency           string
	FiatValue              decimal.Decimal
	FeeAmount              decimal.Decimal
	TenantPayableAmount    decimal.Decimal
	AttemptCount           int
	ConfirmationDeadlineAt *time.Time
	OpsPagedAt             *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// FeeResolver is the one tenant-module capability settlement needs — a
// narrow interface, not internal/tenant.Store directly, same convention
// session.EntitlementChecker/treasury.TenantWebhookLookup already establish
// for keeping the tenant module boundary hard everywhere else touches it.
type FeeResolver interface {
	EffectiveFeeBps(ctx context.Context, tenantID, corridorID uuid.UUID) (int, error)
}

// Config is what main.go builds from *config.Config to construct a Store —
// same convention treasury.Config/rate.Config follow.
type Config struct {
	CNGN        SettlementProviderConfig
	Flutterwave SettlementProviderConfig
	Paystack    SettlementProviderConfig
	Monnify     SettlementProviderConfig
	HydrogenPay SettlementProviderConfig
}

type Store struct {
	pool          *db.Pool
	ledger        *ledger.Ledger
	corridorStore *corridor.Store
	sessionStore  *session.Store
	treasuryStore *treasury.Store
	rateStore     *rate.Store
	feeResolver   FeeResolver

	providers map[string]SettlementProvider

	// bus publishes settlement.dispatched/completed/failed/reversed. Nil-safe
	// — a Store built without one just skips publishing, same convention
	// treasury/session already establish.
	bus *eventbus.Bus
}

func New(pool *db.Pool, ledgerStore *ledger.Ledger, corridorStore *corridor.Store, sessionStore *session.Store, treasuryStore *treasury.Store, rateStore *rate.Store, feeResolver FeeResolver, cfg Config) *Store {
	cngn := newCNGNProvider(cfg.CNGN)
	flutterwave := newFlutterwaveProvider(cfg.Flutterwave)
	paystack := newPaystackProvider(cfg.Paystack)
	monnify := newMonnifyProvider(cfg.Monnify)
	hydrogenpay := newHydrogenPayProvider(cfg.HydrogenPay)
	return &Store{
		pool:          pool,
		ledger:        ledgerStore,
		corridorStore: corridorStore,
		sessionStore:  sessionStore,
		treasuryStore: treasuryStore,
		rateStore:     rateStore,
		feeResolver:   feeResolver,
		providers: map[string]SettlementProvider{
			cngn.Name():        cngn,
			flutterwave.Name(): flutterwave,
			paystack.Name():    paystack,
			monnify.Name():     monnify,
			hydrogenpay.Name(): hydrogenpay,
		},
	}
}

// SetEventBus wires this Store to publish/subscribe to events. Optional —
// nil-safe, see the bus field's doc comment.
func (s *Store) SetEventBus(bus *eventbus.Bus) {
	s.bus = bus
}

const settlementColumns = `id, session_id, tenant_id, corridor_id, status, crypto_asset, crypto_amount,
	fiat_currency, fiat_value, fee_amount, tenant_payable_amount, attempt_count,
	confirmation_deadline_at, ops_paged_at, created_at, updated_at`

func scanSettlement(row pgx.Row) (*Settlement, error) {
	var st Settlement
	var status string
	err := row.Scan(
		&st.ID, &st.SessionID, &st.TenantID, &st.CorridorID, &status, &st.CryptoAsset, &st.CryptoAmount,
		&st.FiatCurrency, &st.FiatValue, &st.FeeAmount, &st.TenantPayableAmount, &st.AttemptCount,
		&st.ConfirmationDeadlineAt, &st.OpsPagedAt, &st.CreatedAt, &st.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	st.Status = Status(status)
	return &st, nil
}

// GetSettlement looks up a settlement by id.
func (s *Store) GetSettlement(ctx context.Context, id uuid.UUID) (*Settlement, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+settlementColumns+` FROM settlements WHERE id = $1`, id)
	st, err := scanSettlement(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("settlement: get: %w", err)
	}
	return st, nil
}

// GetSettlementBySession looks up a settlement by its owning session.
func (s *Store) GetSettlementBySession(ctx context.Context, sessionID uuid.UUID) (*Settlement, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+settlementColumns+` FROM settlements WHERE session_id = $1`, sessionID)
	st, err := scanSettlement(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("settlement: get by session: %w", err)
	}
	return st, nil
}

// ListSettlements returns every settlement in a given status, oldest first —
// the ops queue view behind GET /admin/settlements.
func (s *Store) ListSettlements(ctx context.Context, status Status) ([]Settlement, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+settlementColumns+` FROM settlements WHERE status = $1 ORDER BY created_at`,
		string(status),
	)
	if err != nil {
		return nil, fmt.Errorf("settlement: list: %w", err)
	}
	defer rows.Close()

	var out []Settlement
	for rows.Next() {
		st, err := scanSettlement(rows)
		if err != nil {
			return nil, fmt.Errorf("settlement: list: scan: %w", err)
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}
