// Package compliance is the KYB (tenant onboarding) case flow and, later,
// transaction screening (Phase 5's session module reuses the same case
// model). No screening vendor is selected yet — cases default to a manual
// hold queue, and automated providers slot in later via Registry without
// callers changing. Rolling Travel Rule volume aggregates are a Phase 5
// concern (they only make sense once sessions exist) and are deliberately
// not built here.
package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
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
}

func New(pool *db.Pool, registry *Registry) *Store {
	return &Store{pool: pool, registry: registry}
}

const caseColumns = `id, case_type, reference_type, reference_id, status,
	provider_name, decision_reason, submitted_data, hold_expires_at,
	resolved_by, created_at, resolved_at`

func scanCase(row pgx.Row) (*Case, error) {
	var c Case
	var caseType, status string
	err := row.Scan(
		&c.ID, &caseType, &c.ReferenceType, &c.ReferenceID, &status,
		&c.ProviderName, &c.DecisionReason, &c.SubmittedData, &c.HoldExpiresAt,
		&c.ResolvedBy, &c.CreatedAt, &c.ResolvedAt,
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
func (s *Store) ScreenTenant(ctx context.Context, tenantID uuid.UUID, submittedData json.RawMessage, providerName string) (*Case, error) {
	if submittedData == nil {
		submittedData = json.RawMessage("{}")
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

	row := s.pool.QueryRow(ctx,
		`INSERT INTO compliance_cases (case_type, reference_type, reference_id, status, provider_name, decision_reason, submitted_data, resolved_at)
		 VALUES ($1, 'tenant', $2, $3, $4, $5, $6, $7)
		 RETURNING `+caseColumns,
		string(CaseTypeKYB), tenantID, string(status), resolvedProviderName, decisionReason, submittedData, resolvedAt,
	)
	c, err := scanCase(row)
	if err != nil {
		return nil, fmt.Errorf("compliance: create kyb case: %w", err)
	}
	return c, nil
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
