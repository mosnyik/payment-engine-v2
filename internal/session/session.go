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
	// ErrInvalidDestination wraps corridor.ErrDestinationInvalid so callers
	// can errors.Is against this package alone, same convention as the two
	// errors above.
	ErrInvalidDestination = errors.New("session: payout destination invalid")
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
	// PayoutDestination is tenant-supplied at CreateSession, validated
	// against the corridor's RequiredDestinationFields (ARCHITECTURE.md §8
	// "Payout destination") — never a tenant-level field, since one tenant
	// can hold entitlements across multiple corridors/currencies at once.
	PayoutDestination json.RawMessage
	CreatedAt         time.Time
	UpdatedAt         time.Time
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
	deposit_reservation_id, sla_breached_at, payout_destination, created_at, updated_at`

func scanSession(row pgx.Row) (*Session, error) {
	var sess Session
	var status string
	err := row.Scan(
		&sess.ID, &sess.TenantID, &sess.CorridorID, &status, &sess.FiatCurrency, &sess.FiatAmount,
		&sess.CryptoAsset, &sess.CryptoNetwork, &sess.ComplianceCaseID, &sess.RateLockID,
		&sess.DepositReservationID, &sess.SLABreachedAt, &sess.PayoutDestination, &sess.CreatedAt, &sess.UpdatedAt,
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
func (s *Store) CreateSession(ctx context.Context, tenantID uuid.UUID, cryptoAsset, cryptoNetwork, fiatCurrency string, fiatAmount decimal.Decimal, payoutDestination json.RawMessage) (*Session, error) {
	corr, err := s.corridorStore.GetCorridor(ctx, cryptoAsset, cryptoNetwork, fiatCurrency)
	if errors.Is(err, corridor.ErrNotFound) {
		return nil, ErrCorridorNotSupported
	}
	if err != nil {
		return nil, fmt.Errorf("session: get corridor: %w", err)
	}

	// Validated immediately after the corridor lookup, before entitlement,
	// compliance, rate-lock, or address-reservation ever run — an invalid
	// destination should fail fast, not surface as a dispatch failure at
	// settlement time after crypto has already been collected.
	if err := corr.ValidateDestination(payoutDestination); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDestination, err)
	}
	if payoutDestination == nil {
		payoutDestination = json.RawMessage("{}")
	}

	entitled, err := s.entitlement.CheckEntitlement(ctx, tenantID, corr.ID)
	if err != nil {
		return nil, fmt.Errorf("session: check entitlement: %w", err)
	}
	if !entitled {
		return nil, ErrNotEntitled
	}

	row := s.pool.QueryRow(ctx,
		`INSERT INTO sessions (tenant_id, corridor_id, status, fiat_currency, fiat_amount, crypto_asset, crypto_network, payout_destination)
		 VALUES ($1, $2, 'screening', $3, $4, $5, $6, $7)
		 RETURNING `+sessionColumns,
		tenantID, corr.ID, fiatCurrency, fiatAmount, cryptoAsset, cryptoNetwork, payoutDestination,
	)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("session: create: %w", err)
	}

	// The corridor's highest-priority active compliance provider binding, if
	// any — empty when none is configured, which compliance.ScreenSession
	// already treats as "no vendor selected, manual hold" (same convention
	// ScreenTenant uses). No failover loop here: compliance screening isn't
	// a failover-eligible operation the way treasury/rate provider lookups
	// are, since only one decision is made per session.
	complianceProviderName := ""
	complianceBindings, err := s.corridorStore.ListActiveProviders(ctx, corr.ID, corridor.ProviderTypeCompliance)
	if err != nil {
		return nil, fmt.Errorf("session: list compliance providers: %w", err)
	}
	if len(complianceBindings) > 0 {
		complianceProviderName = complianceBindings[0].ProviderName
	}

	c, err := s.complianceStore.ScreenSession(ctx, sess.ID, tenantID, fiatAmount, corr.TravelRuleThresholdFiat, corr.TravelRuleWindow, corr.ComplianceHoldTimeout, complianceProviderName)
	if err != nil {
		return nil, fmt.Errorf("session: screen: %w", err)
	}

	switch c.Status {
	case compliance.StatusHold:
		return s.transitionToHold(ctx, sess.ID, c.ID)
	case compliance.StatusRejected:
		return s.transitionToRejected(ctx, sess.ID, StatusScreening)
	case compliance.StatusApproved:
		return s.transitionToPending(ctx, sess.ID, tenantID, corr, StatusScreening)
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

// transitionToRejected is reached either directly from the initial
// screening decision or from ResolveComplianceHold's analyst-rejects path
// (ARCHITECTURE.md §8) — from distinguishes which.
func (s *Store) transitionToRejected(ctx context.Context, id uuid.UUID, from Status) (*Session, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE sessions SET status = 'rejected', updated_at = now()
		 WHERE id = $1 AND status = $2
		 RETURNING `+sessionColumns,
		id, string(from),
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
// Reached either directly from the initial screening decision or from
// ResolveComplianceHold's analyst-approves path (ARCHITECTURE.md §8) —
// from distinguishes which.
func (s *Store) transitionToPending(ctx context.Context, id, tenantID uuid.UUID, corr *corridor.Corridor, from Status) (*Session, error) {
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
		 WHERE id = $1 AND status = $4
		 RETURNING `+sessionColumns,
		id, lock.ID, instructions.ReservationID, string(from),
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

// ListSessionsByTenant returns a tenant's own sessions, newest first — the
// portal dashboard's read surface. No admin/ops equivalent exists (admin
// only ever fetches a single session by ID), so this is new, not a
// tenant-scoped variant of an existing list.
func (s *Store) ListSessionsByTenant(ctx context.Context, tenantID uuid.UUID) ([]Session, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE tenant_id = $1 ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("session: list by tenant: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("session: list by tenant: scan: %w", err)
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// ErrNotInComplianceHold means ResolveComplianceHold was called against a
// session that isn't currently held — either it never was, or it already
// moved on (resolved twice, or timed out via the TTL job).
var ErrNotInComplianceHold = errors.New("session: not in compliance_hold")

// ResolveComplianceHold is the admin-facing counterpart to the TTL job's
// own "hold TTL exceeded" transition (ttl.go): an analyst approves or
// rejects a compliance_hold session (ARCHITECTURE.md §8's "analyst
// approves/rejects" transitions). Approving re-runs the same rate-lock +
// deposit-instructions path CreateSession's initial approval does;
// rejecting is a direct compare-and-set. Delegates the underlying
// compliance_cases resolution to compliance.ResolveHold, which already
// enforces its own compare-and-set (can't resolve the same case twice).
func (s *Store) ResolveComplianceHold(ctx context.Context, sessionID, adminID uuid.UUID, approved bool, reason string) (*Session, error) {
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess.Status != StatusComplianceHold || sess.ComplianceCaseID == nil {
		return nil, ErrNotInComplianceHold
	}

	if _, err := s.complianceStore.ResolveHold(ctx, *sess.ComplianceCaseID, adminID, approved, reason); err != nil {
		return nil, fmt.Errorf("session: resolve compliance hold: %w", err)
	}

	if !approved {
		return s.transitionToRejected(ctx, sessionID, StatusComplianceHold)
	}

	corr, err := s.corridorStore.GetCorridorByID(ctx, sess.CorridorID)
	if err != nil {
		return nil, fmt.Errorf("session: get corridor: %w", err)
	}
	return s.transitionToPending(ctx, sessionID, sess.TenantID, corr, StatusComplianceHold)
}

// casTransition is the shared shape for a plain state transition that
// touches only status/updated_at and never publishes an event — unlike
// transitionToPending's session.created publish, every settlement-side
// transition below is driven by the settlement module, which owns
// publishing its own events per ARCHITECTURE.md §5's module map.
func (s *Store) casTransition(ctx context.Context, id uuid.UUID, from, to Status, label string) (*Session, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE sessions SET status = $3, updated_at = now()
		 WHERE id = $1 AND status = $2
		 RETURNING `+sessionColumns,
		id, string(from), string(to),
	)
	sess, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("session: transition to %s: %w", label, err)
	}
	return sess, nil
}

// The settlement-side CAS transitions (ARCHITECTURE.md §8) — called by
// internal/settlement, never internally triggered here, since session has
// no visibility into ledger claims or provider dispatch outcomes.

// TransitionToSettling moves deposit_confirmed -> settling, called once the
// settlement dispatch worker has successfully claimed the settlement_payout
// ledger idempotency key (the fan-out point after deposit_confirmed).
func (s *Store) TransitionToSettling(ctx context.Context, id uuid.UUID) (*Session, error) {
	return s.casTransition(ctx, id, StatusDepositConfirmed, StatusSettling, "settling")
}

// RetryToSettling re-enters settling for an ops-triggered retry — either
// from settlement_failed (auto-retry cap exhausted) or from a still-settling
// row an ops analyst has confirmed genuinely failed (bucket 3, §8). Both
// share one retry policy, so from is parameterized rather than split into
// two near-identical methods.
func (s *Store) RetryToSettling(ctx context.Context, id uuid.UUID, from Status) (*Session, error) {
	return s.casTransition(ctx, id, from, StatusSettling, "retry to settling")
}

// TransitionToSettled moves settling -> settled: the provider's
// signature-verified success webhook has landed (§8 rule 4).
func (s *Store) TransitionToSettled(ctx context.Context, id uuid.UUID) (*Session, error) {
	return s.casTransition(ctx, id, StatusSettling, StatusSettled, "settled")
}

// TransitionToSettlementFailed moves settling -> settlement_failed: bucket 4
// (terminal rejection) hit immediately, or buckets 1/2's 3-attempt auto-retry
// cap exhausted.
func (s *Store) TransitionToSettlementFailed(ctx context.Context, id uuid.UUID) (*Session, error) {
	return s.casTransition(ctx, id, StatusSettling, StatusSettlementFailed, "settlement_failed")
}

// TransitionToReversed moves settled -> reversed: a provider/bank return
// arrived after an initial success (rare, §8).
func (s *Store) TransitionToReversed(ctx context.Context, id uuid.UUID) (*Session, error) {
	return s.casTransition(ctx, id, StatusSettled, StatusReversed, "reversed")
}

// TransitionToReversalResolved moves reversed -> reversal_resolved: ops
// closed the reversal case outside the automated settlement pipeline.
func (s *Store) TransitionToReversalResolved(ctx context.Context, id uuid.UUID) (*Session, error) {
	return s.casTransition(ctx, id, StatusReversed, StatusReversalResolved, "reversal_resolved")
}
