// Package compliance is the KYB (tenant onboarding) case flow and
// transaction screening (Phase 5's session module reuses the same case
// model via ScreenSession). No screening vendor is selected yet — cases
// default to a manual hold queue, and automated providers slot in later via
// Registry without callers changing. Rolling Travel Rule volume is checked
// directly against the sessions table (see travelRuleThresholdExceeded) —
// no separate aggregates table, since the data already lives there.
package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

type CaseType string

const (
	CaseTypeKYB                  CaseType = "kyb"
	CaseTypeTransactionScreening CaseType = "transaction_screening"
)

type Status string

const (
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
	StatusHold     Status = "hold"
)

var (
	ErrNotFound            = errors.New("compliance: case not found")
	ErrHoldAlreadyResolved = errors.New("compliance: hold already resolved")
	ErrProviderNotFound    = errors.New("compliance: provider not registered")
	// ErrNoDeclaredCurrencies is returned by ScreenTenant when a KYB
	// submission declares no jurisdictions at all -- Phase 10: every KYB
	// case must state which fiat currencies/jurisdictions it's onboarding
	// for, since required documentation varies by regulator.
	ErrNoDeclaredCurrencies = errors.New("compliance: no declared currencies")
	// ErrMissingRequiredFields is returned by ScreenTenant when
	// submitted_data is missing a field the declared jurisdiction(s)
	// require -- same shape as corridor.ErrDestinationInvalid.
	ErrMissingRequiredFields = errors.New("compliance: submission missing required fields")
)

type Case struct {
	ID             uuid.UUID
	CaseType       CaseType
	ReferenceType  string
	ReferenceID    uuid.UUID
	Status         Status
	ProviderName   *string
	DecisionReason *string
	SubmittedData  json.RawMessage
	HoldExpiresAt  *time.Time
	ResolvedBy     *uuid.UUID
	CreatedAt      time.Time
	ResolvedAt     *time.Time
	// DeclaredCurrencies is which fiat currencies/jurisdictions a KYB case
	// is onboarding the tenant for (Phase 10) -- always empty for
	// transaction-screening cases.
	DeclaredCurrencies []string
}

type Decision struct {
	Approved bool
	Reason   string
}

// ScreeningProvider is implemented by a real KYB/AML vendor integration.
// None are registered by default.
type ScreeningProvider interface {
	Name() string
	Screen(ctx context.Context, c Case) (Decision, error)
}

// Registry holds registered screening providers by name. A case referencing
// a provider name that isn't registered here falls back to the manual hold
// queue rather than erroring the whole flow — see Store.ScreenTenant.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]ScreeningProvider
}

func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]ScreeningProvider)}
}

func (r *Registry) Register(p ScreeningProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
}

func (r *Registry) Get(name string) (ScreeningProvider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

type Store struct {
	pool     *db.Pool
	registry *Registry

	// bus publishes compliance.hold_created for notification (Phase 7) to
	// alert ops the moment a case is held. Nil-safe — a Store built without
	// one just skips publishing, same convention treasury/settlement/session
	// already establish for their own bus fields.
	bus *eventbus.Bus
}

func New(pool *db.Pool, registry *Registry) *Store {
	return &Store{pool: pool, registry: registry}
}

// SetEventBus wires this Store to publish compliance.hold_created. Optional
// — nil-safe, see the bus field's doc comment.
func (s *Store) SetEventBus(bus *eventbus.Bus) {
	s.bus = bus
}

const caseColumns = `id, case_type, reference_type, reference_id, status,
	provider_name, decision_reason, submitted_data, hold_expires_at,
	resolved_by, created_at, resolved_at, declared_currencies`

func scanCase(row pgx.Row) (*Case, error) {
	var c Case
	var caseType, status string
	err := row.Scan(
		&c.ID, &caseType, &c.ReferenceType, &c.ReferenceID, &status,
		&c.ProviderName, &c.DecisionReason, &c.SubmittedData, &c.HoldExpiresAt,
		&c.ResolvedBy, &c.CreatedAt, &c.ResolvedAt, &c.DeclaredCurrencies,
	)
	if err != nil {
		return nil, err
	}
	c.CaseType = CaseType(caseType)
	c.Status = Status(status)
	return &c, nil
}

// ScreenTenant creates a KYB case for tenantID and immediately resolves it:
// if providerName is non-empty and registered, uses its decision; otherwise
// (empty name, or a name with no registered provider — since none exist
// yet by default) the case lands in the manual hold queue, unresolved,
// with no hold_expires_at (see the schema comment for why).
//
// declaredCurrencies is which fiat currencies/jurisdictions this KYB
// submission is onboarding the tenant for (Phase 10) — required, and
// validated against jurisdiction_kyb_requirements before anything else
// happens: an empty list, or submittedData missing a field the union of
// those jurisdictions' requirements demands, rejects the submission
// outright (no case row inserted, no provider called).
func (s *Store) ScreenTenant(ctx context.Context, tenantID uuid.UUID, submittedData json.RawMessage, declaredCurrencies []string, providerName string) (*Case, error) {
	if submittedData == nil {
		submittedData = json.RawMessage("{}")
	}
	if err := s.validateDeclaredCurrencies(ctx, declaredCurrencies, submittedData); err != nil {
		return nil, err
	}

	status := StatusHold
	var resolvedProviderName, decisionReason *string
	var resolvedAt *time.Time

	if providerName != "" {
		provider, ok := s.registry.Get(providerName)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrProviderNotFound, providerName)
		}
		decision, err := provider.Screen(ctx, Case{
			CaseType:      CaseTypeKYB,
			ReferenceType: "tenant",
			ReferenceID:   tenantID,
			SubmittedData: submittedData,
		})
		if err != nil {
			return nil, fmt.Errorf("compliance: screen: %w", err)
		}
		if decision.Approved {
			status = StatusApproved
		} else {
			status = StatusRejected
		}
		resolvedProviderName = &providerName
		decisionReason = &decision.Reason
		now := time.Now()
		resolvedAt = &now
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("compliance: begin create kyb case: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	row := tx.QueryRow(ctx,
		`INSERT INTO compliance_cases (case_type, reference_type, reference_id, status, provider_name, decision_reason, submitted_data, resolved_at, declared_currencies)
		 VALUES ($1, 'tenant', $2, $3, $4, $5, $6, $7, $8)
		 RETURNING `+caseColumns,
		string(CaseTypeKYB), tenantID, string(status), resolvedProviderName, decisionReason, submittedData, resolvedAt, declaredCurrencies,
	)
	c, err := scanCase(row)
	if err != nil {
		return nil, fmt.Errorf("compliance: create kyb case: %w", err)
	}

	if err := s.publishIfHold(ctx, tx, c, tenantID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("compliance: commit create kyb case: %w", err)
	}
	return c, nil
}

// ScreenSession creates a transaction-screening case for a session created
// by Phase 5's session module. Unlike ScreenTenant, the Travel Rule check
// runs before any provider decision: rolling fiat volume within
// travelRuleWindow (this session's amount plus every non-rejected session
// the tenant created in that window, summed directly against the sessions
// table) forces a hold regardless of what a provider would have decided.
// Hold cases get hold_expires_at populated from holdTimeout (unlike
// ScreenTenant's KYB holds, which have no session-like TTL) — this is the
// corridor's compliance_hold_timeout, which the session module supplies.
func (s *Store) ScreenSession(ctx context.Context, sessionID, tenantID uuid.UUID, fiatAmount decimal.Decimal, travelRuleThresholdFiat *decimal.Decimal, travelRuleWindow, holdTimeout time.Duration, providerName string) (*Case, error) {
	travelRuleHit, err := s.travelRuleThresholdExceeded(ctx, tenantID, fiatAmount, travelRuleThresholdFiat, travelRuleWindow)
	if err != nil {
		return nil, err
	}

	status := StatusHold
	var resolvedProviderName, decisionReason *string
	var resolvedAt *time.Time

	switch {
	case travelRuleHit:
		reason := "travel rule rolling volume threshold exceeded"
		decisionReason = &reason
	case providerName != "":
		provider, ok := s.registry.Get(providerName)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrProviderNotFound, providerName)
		}
		decision, err := provider.Screen(ctx, Case{
			CaseType:      CaseTypeTransactionScreening,
			ReferenceType: "session",
			ReferenceID:   sessionID,
		})
		if err != nil {
			return nil, fmt.Errorf("compliance: screen: %w", err)
		}
		if decision.Approved {
			status = StatusApproved
		} else {
			status = StatusRejected
		}
		resolvedProviderName = &providerName
		decisionReason = &decision.Reason
		now := time.Now()
		resolvedAt = &now
	}

	var holdExpiresAt *time.Time
	if status == StatusHold {
		t := time.Now().Add(holdTimeout)
		holdExpiresAt = &t
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("compliance: begin create session case: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	row := tx.QueryRow(ctx,
		`INSERT INTO compliance_cases (case_type, reference_type, reference_id, status, provider_name, decision_reason, submitted_data, hold_expires_at, resolved_at)
		 VALUES ($1, 'session', $2, $3, $4, $5, $6, $7, $8)
		 RETURNING `+caseColumns,
		string(CaseTypeTransactionScreening), sessionID, string(status), resolvedProviderName, decisionReason, json.RawMessage("{}"), holdExpiresAt, resolvedAt,
	)
	c, err := scanCase(row)
	if err != nil {
		return nil, fmt.Errorf("compliance: create session case: %w", err)
	}

	if err := s.publishIfHold(ctx, tx, c, tenantID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("compliance: commit create session case: %w", err)
	}
	return c, nil
}

// validateDeclaredCurrencies enforces Phase 10's KYB precondition: rejects
// (before any case row is inserted or provider called) if declaredCurrencies
// is empty, or if submittedData is missing a field required by the union of
// those currencies' jurisdiction_kyb_requirements. Mirrors
// corridor.Corridor.ValidateDestination's missing-field logic and error
// shape exactly.
func (s *Store) validateDeclaredCurrencies(ctx context.Context, declaredCurrencies []string, submittedData json.RawMessage) error {
	if len(declaredCurrencies) == 0 {
		return ErrNoDeclaredCurrencies
	}

	seen := make(map[string]bool)
	var required []string
	for _, currency := range declaredCurrencies {
		fields, err := s.GetRequiredFields(ctx, currency)
		if err != nil {
			return err
		}
		for _, f := range fields {
			if !seen[f] {
				seen[f] = true
				required = append(required, f)
			}
		}
	}
	if len(required) == 0 {
		return nil
	}

	var data map[string]any
	if len(submittedData) > 0 {
		if err := json.Unmarshal(submittedData, &data); err != nil {
			return fmt.Errorf("%w: not a JSON object: %v", ErrMissingRequiredFields, err)
		}
	}
	var missing []string
	for _, req := range required {
		v, ok := data[req]
		if !ok {
			missing = append(missing, req)
			continue
		}
		s, ok := v.(string)
		if !ok || s == "" {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing or empty field(s): %s", ErrMissingRequiredFields, strings.Join(missing, ", "))
	}
	return nil
}

// publishIfHold publishes compliance.hold_created in the same transaction
// as the case insert above when the case landed in the manual hold queue —
// the moment ops actually needs to know about it, not KYB/session
// approval or rejection. tenant_id is a parameter (not read off c) because
// ScreenTenant's reference_id already *is* the tenant id, while
// ScreenSession's reference_id is the session id — both callers already
// have the tenant id in hand regardless.
func (s *Store) publishIfHold(ctx context.Context, tx pgx.Tx, c *Case, tenantID uuid.UUID) error {
	if s.bus == nil || c.Status != StatusHold {
		return nil
	}
	payload, err := json.Marshal(map[string]string{
		"tenant_id":      tenantID.String(),
		"case_type":      string(c.CaseType),
		"reference_type": c.ReferenceType,
		"reference_id":   c.ReferenceID.String(),
	})
	if err != nil {
		return fmt.Errorf("compliance: marshal compliance.hold_created payload: %w", err)
	}
	if err := s.bus.Publish(ctx, tx, eventbus.Event{
		EventType:     "compliance.hold_created",
		AggregateType: "compliance_case",
		AggregateID:   c.ID,
		Payload:       payload,
	}); err != nil {
		return fmt.Errorf("compliance: publish compliance.hold_created: %w", err)
	}
	return nil
}

// travelRuleThresholdExceeded reports whether fiatAmount plus the tenant's
// rolling non-rejected session volume within window meets or exceeds
// threshold. A nil threshold (corridor has none configured) always returns
// false.
func (s *Store) travelRuleThresholdExceeded(ctx context.Context, tenantID uuid.UUID, fiatAmount decimal.Decimal, threshold *decimal.Decimal, window time.Duration) (bool, error) {
	if threshold == nil {
		return false, nil
	}
	var priorVolume decimal.Decimal
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(fiat_amount), 0) FROM sessions
		 WHERE tenant_id = $1 AND created_at >= $2 AND status <> 'rejected'`,
		tenantID, time.Now().Add(-window),
	).Scan(&priorVolume)
	if err != nil {
		return false, fmt.Errorf("compliance: travel rule volume: %w", err)
	}
	return priorVolume.Add(fiatAmount).GreaterThanOrEqual(*threshold), nil
}

func (s *Store) GetCase(ctx context.Context, id uuid.UUID) (*Case, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+caseColumns+` FROM compliance_cases WHERE id = $1`, id)
	c, err := scanCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("compliance: get case: %w", err)
	}
	return c, nil
}

// GetLatestCase returns the most recently created case for a given
// reference (e.g. the current KYB case for a tenant) — useful for checking
// "where does this tenant's onboarding currently stand".
func (s *Store) GetLatestCase(ctx context.Context, referenceType string, referenceID uuid.UUID, caseType CaseType) (*Case, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+caseColumns+` FROM compliance_cases
		 WHERE reference_type = $1 AND reference_id = $2 AND case_type = $3
		 ORDER BY created_at DESC LIMIT 1`,
		referenceType, referenceID, string(caseType),
	)
	c, err := scanCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("compliance: get latest case: %w", err)
	}
	return c, nil
}

// ListHolds returns open hold-queue cases of the given type, oldest first —
// this is the ops queue surface (exposed via platform/adminauth-gated
// endpoints when the onboarding workflow wires it in).
func (s *Store) ListHolds(ctx context.Context, caseType CaseType) ([]Case, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+caseColumns+` FROM compliance_cases
		 WHERE case_type = $1 AND status = 'hold'
		 ORDER BY created_at ASC`,
		string(caseType),
	)
	if err != nil {
		return nil, fmt.Errorf("compliance: list holds: %w", err)
	}
	defer rows.Close()

	var cases []Case
	for rows.Next() {
		c, err := scanCase(rows)
		if err != nil {
			return nil, fmt.Errorf("compliance: scan hold: %w", err)
		}
		cases = append(cases, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("compliance: list holds: %w", err)
	}
	return cases, nil
}

// ResolveHold is the manual review action: an admin approves or rejects a
// held case. Compare-and-set on status='hold' — the same discipline used
// for every other status transition in this codebase — so a hold can't be
// resolved twice (e.g. two admins acting on the same case concurrently).
func (s *Store) ResolveHold(ctx context.Context, caseID, adminID uuid.UUID, approved bool, reason string) (*Case, error) {
	status := StatusRejected
	if approved {
		status = StatusApproved
	}

	row := s.pool.QueryRow(ctx,
		`UPDATE compliance_cases
		 SET status = $2, decision_reason = $3, resolved_by = $4, resolved_at = now(), provider_name = 'manual'
		 WHERE id = $1 AND status = 'hold'
		 RETURNING `+caseColumns,
		caseID, string(status), reason, adminID,
	)
	c, err := scanCase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrHoldAlreadyResolved
	}
	if err != nil {
		return nil, fmt.Errorf("compliance: resolve hold: %w", err)
	}
	return c, nil
}
