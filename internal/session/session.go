// Package session is Phase 5's orchestrator: corridor lookup ->
// compliance.ScreenSession() -> rate.LockRate() ->
// treasury.GetDepositInstructions(), driven by the state machine designed
// in ARCHITECTURE.md §8. Every transition is a compare-and-set
// (UPDATE ... WHERE status = <expected>), the same discipline every other
// module in this codebase already uses — the direct structural fix for
// v1's non-atomic settlement bug, applied here from the start rather than
// retrofitted later.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
	"github.com/sirfi/payment-engine-v2/internal/rate"
	"github.com/sirfi/payment-engine-v2/internal/treasury"
)

// Status is one state in the session state machine (ARCHITECTURE.md §8).
// The full 11-state enum is modeled now even though settling/settled/
// settlement_failed/reversed/reversal_resolved aren't driven until Phase 6
// (settlement) exists — the state machine itself is already fully designed.
type Status string

const (
	StatusScreening        Status = "screening"
	StatusComplianceHold   Status = "compliance_hold"
	StatusRejected         Status = "rejected"
	StatusPending          Status = "pending"
	StatusExpired          Status = "expired"
	StatusDepositDetected  Status = "deposit_detected"
	StatusDepositConfirmed Status = "deposit_confirmed"
	StatusSettling         Status = "settling"
	StatusSettled          Status = "settled"
	StatusSettlementFailed Status = "settlement_failed"
	StatusReversed         Status = "reversed"
	StatusReversalResolved Status = "reversal_resolved"
)

// SessionTTL is the hard pre-deposit expiry / post-deposit SLA line
// (ARCHITECTURE.md §8's "one firm 30-minute promise") — a fixed design
// decision, not an ops knob, same convention as rate.slippageBuffer.
const SessionTTL = 30 * time.Minute

var (
	ErrNotFound             = errors.New("session: not found")
	ErrCorridorNotSupported = errors.New("session: corridor not supported")
	ErrNotEntitled          = errors.New("session: tenant not entitled to this corridor")
)

// EntitlementChecker is the one tenant-module capability session needs —
// a narrow interface, not internal/tenant.Store directly, so the module
// boundary stays a hard one (same convention treasury.TenantWebhookLookup
// already establishes for tenant/treasury).
type EntitlementChecker interface {
	CheckEntitlement(ctx context.Context, tenantID, corridorID uuid.UUID) (bool, error)
}

type Session struct {
	ID                   uuid.UUID
	TenantID             uuid.UUID
	CorridorID           uuid.UUID
	Status               Status
	FiatCurrency         string
	FiatAmount           decimal.Decimal
	CryptoAsset          string
	CryptoNetwork        string
	ComplianceCaseID     *uuid.UUID
	RateLockID           *uuid.UUID
	DepositReservationID *uuid.UUID
	SLABreachedAt        *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Store struct {
	pool            *db.Pool
	corridorStore   *corridor.Store
	complianceStore *compliance.Store
	rateStore       *rate.Store
	treasuryStore   *treasury.Store
	entitlement     EntitlementChecker

	// bus publishes session.created/deposit_detected/deposit_confirmed and
	// drives the deposit-side transitions in events.go. Nil-safe — a Store
	// built without one just skips publishing (same convention
	// treasury.Store.bus already established), which is what a pure
	// state-machine test that doesn't care about events can do.
	bus *eventbus.Bus
}

func New(pool *db.Pool, corridorStore *corridor.Store, complianceStore *compliance.Store, rateStore *rate.Store, treasuryStore *treasury.Store, entitlement EntitlementChecker, bus *eventbus.Bus) *Store {
	return &Store{
		pool:            pool,
		corridorStore:   corridorStore,
		complianceStore: complianceStore,
		rateStore:       rateStore,
		treasuryStore:   treasuryStore,
		entitlement:     entitlement,
		bus:             bus,
	}
}

const sessionColumns = `id, tenant_id, corridor_id, status, fiat_currency, fiat_amount,
	crypto_asset, crypto_network, compliance_case_id, rate_lock_id,
	deposit_reservation_id, sla_breached_at, created_at, updated_at`

func scanSession(row pgx.Row) (*Session, error) {
	var sess Session
	var status string
	err := row.Scan(
		&sess.ID, &sess.TenantID, &sess.CorridorID, &status, &sess.FiatCurrency, &sess.FiatAmount,
		&sess.CryptoAsset, &sess.CryptoNetwork, &sess.ComplianceCaseID, &sess.RateLockID,
		&sess.DepositReservationID, &sess.SLABreachedAt, &sess.CreatedAt, &sess.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	sess.Status = Status(status)
	return &sess, nil
}

// CreateSession is the orchestration entry point: corridor lookup ->
// entitlement check -> compliance.ScreenSession() -> (approved:
// rate.LockRate() + treasury.GetDepositInstructions()) -> pending, or hold/
// rejected per the compliance decision.
func (s *Store) CreateSession(ctx context.Context, tenantID uuid.UUID, cryptoAsset, cryptoNetwork, fiatCurrency string, fiatAmount decimal.Decimal) (*Session, error) {
	corr, err := s.corridorStore.GetCorridor(ctx, cryptoAsset, cryptoNetwork, fiatCurrency)
	if errors.Is(err, corridor.ErrNotFound) {
		return nil, ErrCorridorNotSupported
	}
	if err != nil {
		return nil, fmt.Errorf("session: get corridor: %w", err)
	}

	entitled, err := s.entitlement.CheckEntitlement(ctx, tenantID, corr.ID)
	if err != nil {
		return nil, fmt.Errorf("session: check entitlement: %w", err)
	}
	if !entitled {
		return nil, ErrNotEntitled
	}

	row := s.pool.QueryRow(ctx,
		`INSERT INTO sessions (tenant_id, corridor_id, status, fiat_currency, fiat_amount, crypto_asset, crypto_network)
		 VALUES ($1, $2, 'screening', $3, $4, $5, $6)
		 RETURNING `+sessionColumns,
		tenantID, corr.ID, fiatCurrency, fiatAmount, cryptoAsset, cryptoNetwork,
	)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("session: create: %w", err)
	}

	c, err := s.complianceStore.ScreenSession(ctx, sess.ID, tenantID, fiatAmount, corr.TravelRuleThresholdFiat, corr.TravelRuleWindow, corr.ComplianceHoldTimeout, "")
	if err != nil {
		return nil, fmt.Errorf("session: screen: %w", err)
	}

	switch c.Status {
	case compliance.StatusHold:
		return s.transitionToHold(ctx, sess.ID, c.ID)
	case compliance.StatusRejected:
		return s.transitionToRejected(ctx, sess.ID)
	case compliance.StatusApproved:
		return s.transitionToPending(ctx, sess.ID, tenantID, corr)
	default:
		return nil, fmt.Errorf("session: unexpected compliance status %q", c.Status)
	}
}

func (s *Store) transitionToHold(ctx context.Context, id, caseID uuid.UUID) (*Session, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE sessions SET status = 'compliance_hold', compliance_case_id = $2, updated_at = now()
		 WHERE id = $1 AND status = 'screening'
		 RETURNING `+sessionColumns,
		id, caseID,
	)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("session: transition to compliance_hold: %w", err)
	}
	return sess, nil
}

func (s *Store) transitionToRejected(ctx context.Context, id uuid.UUID) (*Session, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE sessions SET status = 'rejected', updated_at = now()
		 WHERE id = $1 AND status = 'screening'
		 RETURNING `+sessionColumns,
		id,
	)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("session: transition to rejected: %w", err)
	}
	return sess, nil
}

// transitionToPending locks a rate and reserves a deposit address, then
// commits the pending transition and the session.created event in one
// transaction — the first real eventbus.Publish call on the request path.
func (s *Store) transitionToPending(ctx context.Context, id, tenantID uuid.UUID, corr *corridor.Corridor) (*Session, error) {
	lock, err := s.rateStore.LockRate(ctx, corr.CryptoAsset, corr.FiatCurrency, rate.DefaultLockTTL)
	if err != nil {
		return nil, fmt.Errorf("session: lock rate: %w", err)
	}
	instructions, err := s.treasuryStore.GetDepositInstructions(ctx, tenantID, corr.ID)
	if err != nil {
		return nil, fmt.Errorf("session: get deposit instructions: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("session: begin pending transition: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	row := tx.QueryRow(ctx,
		`UPDATE sessions SET status = 'pending', rate_lock_id = $2, deposit_reservation_id = $3, updated_at = now()
		 WHERE id = $1 AND status = 'screening'
		 RETURNING `+sessionColumns,
		id, lock.ID, instructions.ReservationID,
	)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("session: transition to pending: %w", err)
	}

	if s.bus != nil {
		payload, err := json.Marshal(map[string]string{
			"tenant_id":   tenantID.String(),
			"corridor_id": corr.ID.String(),
		})
		if err != nil {
			return nil, fmt.Errorf("session: marshal session.created payload: %w", err)
		}
		if err := s.bus.Publish(ctx, tx, eventbus.Event{
			EventType:     "session.created",
			AggregateType: "session",
			AggregateID:   id,
			Payload:       payload,
		}); err != nil {
			return nil, fmt.Errorf("session: publish session.created: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("session: commit pending transition: %w", err)
	}
	return sess, nil
}

// GetSession looks up a session by id.
func (s *Store) GetSession(ctx context.Context, id uuid.UUID) (*Session, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = $1`, id)
	sess, err := scanSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("session: get: %w", err)
	}
	return sess, nil
}
