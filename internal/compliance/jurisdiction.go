package compliance

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// JurisdictionRequirement is which KYB documentation fields a fiat
// currency's regulator requires (Phase 10). No row for a currency means
// nothing required yet -- see GetRequiredFields.
type JurisdictionRequirement struct {
	ID             uuid.UUID
	FiatCurrency   string
	Jurisdiction   string
	RequiredFields []string
}

// UpsertJurisdictionRequirement creates or updates the requirement row for
// fiatCurrency -- config-driven, same upsert-by-natural-key shape as
// corridor.Store.UpsertCorridor.
func (s *Store) UpsertJurisdictionRequirement(ctx context.Context, fiatCurrency, jurisdiction string, requiredFields []string) (uuid.UUID, error) {
	if requiredFields == nil {
		requiredFields = []string{}
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO jurisdiction_kyb_requirements (fiat_currency, jurisdiction, required_fields)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (fiat_currency) DO UPDATE SET
			jurisdiction = EXCLUDED.jurisdiction,
			required_fields = EXCLUDED.required_fields,
			updated_at = now()
		 RETURNING id`,
		fiatCurrency, jurisdiction, requiredFields,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("compliance: upsert jurisdiction requirement: %w", err)
	}
	return id, nil
}

// GetRequiredFields returns the configured required_fields for fiatCurrency,
// or an empty slice if no row exists -- unconfigured currencies impose no
// requirement, same default-permissive convention
// corridors.required_destination_fields already uses.
func (s *Store) GetRequiredFields(ctx context.Context, fiatCurrency string) ([]string, error) {
	var fields []string
	err := s.pool.QueryRow(ctx,
		`SELECT required_fields FROM jurisdiction_kyb_requirements WHERE fiat_currency = $1`,
		fiatCurrency,
	).Scan(&fields)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("compliance: get required fields: %w", err)
	}
	return fields, nil
}

// ListJurisdictionRequirements returns every configured jurisdiction
// requirement -- ops visibility surface.
func (s *Store) ListJurisdictionRequirements(ctx context.Context) ([]JurisdictionRequirement, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, fiat_currency, jurisdiction, required_fields FROM jurisdiction_kyb_requirements ORDER BY fiat_currency`,
	)
	if err != nil {
		return nil, fmt.Errorf("compliance: list jurisdiction requirements: %w", err)
	}
	defer rows.Close()

	var reqs []JurisdictionRequirement
	for rows.Next() {
		var r JurisdictionRequirement
		if err := rows.Scan(&r.ID, &r.FiatCurrency, &r.Jurisdiction, &r.RequiredFields); err != nil {
			return nil, fmt.Errorf("compliance: scan jurisdiction requirement: %w", err)
		}
		reqs = append(reqs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("compliance: list jurisdiction requirements: %w", err)
	}
	return reqs, nil
}

// IsJurisdictionApproved reports whether tenantID has an approved KYB case
// whose declared_currencies covers fiatCurrency -- what
// tenant.GrantCorridorEntitlement checks before activating an entitlement
// on a corridor for that currency (Phase 10 item 3).
func (s *Store) IsJurisdictionApproved(ctx context.Context, tenantID uuid.UUID, fiatCurrency string) (bool, error) {
	var approved bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
			SELECT 1 FROM compliance_cases
			WHERE reference_type = 'tenant' AND reference_id = $1
			  AND case_type = 'kyb' AND status = 'approved'
			  AND $2 = ANY(declared_currencies)
		 )`,
		tenantID, fiatCurrency,
	).Scan(&approved)
	if err != nil {
		return false, fmt.Errorf("compliance: is jurisdiction approved: %w", err)
	}
	return approved, nil
}
