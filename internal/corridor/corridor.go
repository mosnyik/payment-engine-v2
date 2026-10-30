// Package corridor is the config entity binding a crypto asset+network to a
// fiat currency: which providers serve it (with failover priority), limits,
// and Travel Rule / compliance-hold thresholds. Adding a new fiat, crypto,
// or provider binding is a row in this module's tables, not a redeploy.
package corridor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

var (
	ErrNotFound = errors.New("corridor: not found")
	// ErrDestinationInvalid is returned by ValidateDestination when a
	// tenant-supplied payout destination is missing a field this corridor
	// requires (ARCHITECTURE.md §8 "Payout destination").
	ErrDestinationInvalid = errors.New("corridor: payout destination invalid")
	// ErrAlreadyExists is returned by CreateCorridor when a corridor with the
	// same (crypto_asset, crypto_network, fiat_currency) already exists, or
	// when one of its provider bindings is duplicated.
	ErrAlreadyExists = errors.New("corridor: already exists")
)

type Corridor struct {
	ID                        uuid.UUID
	CryptoAsset               string
	CryptoNetwork             string
	FiatCurrency              string
	Active                    bool
	MinAmountFiat             *decimal.Decimal
	MaxAmountFiat             *decimal.Decimal
	TravelRuleThresholdFiat   *decimal.Decimal
	TravelRuleWindow          time.Duration
	ComplianceHoldTimeout     time.Duration
	RequiredDestinationFields []string
}

// ValidateDestination checks that dest (the tenant-supplied payout
// destination from CreateSession) carries every field this corridor
// requires, each with a non-empty string value. dest is otherwise opaque —
// corridors only declare which keys must be present, not the full shape,
// since that varies by settlement provider.
func (c *Corridor) ValidateDestination(dest json.RawMessage) error {
	if len(c.RequiredDestinationFields) == 0 {
		return nil
	}
	var fields map[string]any
	if len(dest) > 0 {
		if err := json.Unmarshal(dest, &fields); err != nil {
			return fmt.Errorf("%w: not a JSON object: %v", ErrDestinationInvalid, err)
		}
	}
	var missing []string
	for _, req := range c.RequiredDestinationFields {
		v, ok := fields[req]
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
		return fmt.Errorf("%w: missing or empty field(s): %s", ErrDestinationInvalid, strings.Join(missing, ", "))
	}
	return nil
}

type ProviderType string

const (
	ProviderTypeCollection ProviderType = "collection"
	ProviderTypeSettlement ProviderType = "settlement"
	ProviderTypeCompliance ProviderType = "compliance"
	ProviderTypeRate       ProviderType = "rate"
)

type ProviderBinding struct {
	ID           uuid.UUID
	CorridorID   uuid.UUID
	ProviderType ProviderType
	ProviderName string
	Priority     int
	Active       bool
	Config       json.RawMessage
}

type Store struct {
	pool *db.Pool
}

func New(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

const corridorColumns = `id, crypto_asset, crypto_network, fiat_currency, active,
	min_amount_fiat, max_amount_fiat, travel_rule_threshold_fiat,
	travel_rule_window_seconds, compliance_hold_timeout_seconds, required_destination_fields`

func scanCorridor(row pgx.Row) (*Corridor, error) {
	var c Corridor
	var travelRuleWindowSec, holdTimeoutSec int
	err := row.Scan(
		&c.ID, &c.CryptoAsset, &c.CryptoNetwork, &c.FiatCurrency, &c.Active,
		&c.MinAmountFiat, &c.MaxAmountFiat, &c.TravelRuleThresholdFiat,
		&travelRuleWindowSec, &holdTimeoutSec, &c.RequiredDestinationFields,
	)
	if err != nil {
		return nil, err
	}
	c.TravelRuleWindow = time.Duration(travelRuleWindowSec) * time.Second
	c.ComplianceHoldTimeout = time.Duration(holdTimeoutSec) * time.Second
	return &c, nil
}

// GetCorridor looks up an active corridor by its natural key. Returns
// ErrNotFound if no active corridor matches — callers (e.g. session
// creation) should treat that as "this combination isn't supported", not
// retry.
func (s *Store) GetCorridor(ctx context.Context, cryptoAsset, cryptoNetwork, fiatCurrency string) (*Corridor, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+corridorColumns+` FROM corridors
		 WHERE crypto_asset = $1 AND crypto_network = $2 AND fiat_currency = $3 AND active`,
		cryptoAsset, cryptoNetwork, fiatCurrency,
	)
	c, err := scanCorridor(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corridor: get corridor: %w", err)
	}
	return c, nil
}

func (s *Store) GetCorridorByID(ctx context.Context, id uuid.UUID) (*Corridor, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+corridorColumns+` FROM corridors WHERE id = $1`, id)
	c, err := scanCorridor(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corridor: get corridor by id: %w", err)
	}
	return c, nil
}

// ListCorridors is the admin browse surface (Phase 11) — every corridor,
// newest first, optionally restricted to active ones. Config data, not an
// append-only log, so it's expected to stay small, but paginated for
// consistency with the other new list endpoints (limit<=0 means unbounded).
func (s *Store) ListCorridors(ctx context.Context, activeOnly bool, limit, offset int) ([]Corridor, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM corridors WHERE (NOT $1 OR active)`, activeOnly,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("corridor: list corridors: count: %w", err)
	}

	query := `SELECT ` + corridorColumns + ` FROM corridors WHERE (NOT $1 OR active)
		ORDER BY created_at DESC`
	args := []any{activeOnly}
	if limit > 0 {
		query += ` LIMIT $2 OFFSET $3`
		args = append(args, limit, offset)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("corridor: list corridors: %w", err)
	}
	defer rows.Close()

	var out []Corridor
	for rows.Next() {
		c, err := scanCorridor(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("corridor: list corridors: scan: %w", err)
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("corridor: list corridors: %w", err)
	}
	return out, total, nil
}

// FiatCurrencyForCorridor returns a corridor's fiat currency — the one
// piece of corridor data tenant.GrantCorridorEntitlement needs (Phase 10),
// exposed narrowly rather than requiring that caller to depend on the full
// Corridor type. Satisfies tenant.FiatCurrencyLookup directly.
func (s *Store) FiatCurrencyForCorridor(ctx context.Context, id uuid.UUID) (string, error) {
	c, err := s.GetCorridorByID(ctx, id)
	if err != nil {
		return "", err
	}
	return c.FiatCurrency, nil
}

type UpsertCorridorInput struct {
	CryptoAsset               string
	CryptoNetwork             string
	FiatCurrency              string
	Active                    bool
	MinAmountFiat             *decimal.Decimal
	MaxAmountFiat             *decimal.Decimal
	TravelRuleThresholdFiat   *decimal.Decimal
	TravelRuleWindow          time.Duration
	ComplianceHoldTimeout     time.Duration
	RequiredDestinationFields []string
}

// UpsertCorridor creates or updates the corridor matching (crypto_asset,
// crypto_network, fiat_currency) — this is the config-driven mechanism for
// adding/adjusting a corridor without a redeploy.
func (s *Store) UpsertCorridor(ctx context.Context, in UpsertCorridorInput) (uuid.UUID, error) {
	fields := in.RequiredDestinationFields
	if fields == nil {
		fields = []string{}
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO corridors (
			crypto_asset, crypto_network, fiat_currency, active,
			min_amount_fiat, max_amount_fiat, travel_rule_threshold_fiat,
			travel_rule_window_seconds, compliance_hold_timeout_seconds,
			required_destination_fields
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (crypto_asset, crypto_network, fiat_currency) DO UPDATE SET
			active = EXCLUDED.active,
			min_amount_fiat = EXCLUDED.min_amount_fiat,
			max_amount_fiat = EXCLUDED.max_amount_fiat,
			travel_rule_threshold_fiat = EXCLUDED.travel_rule_threshold_fiat,
			travel_rule_window_seconds = EXCLUDED.travel_rule_window_seconds,
			compliance_hold_timeout_seconds = EXCLUDED.compliance_hold_timeout_seconds,
			required_destination_fields = EXCLUDED.required_destination_fields,
			updated_at = now()
		 RETURNING id`,
		in.CryptoAsset, in.CryptoNetwork, in.FiatCurrency, in.Active,
		in.MinAmountFiat, in.MaxAmountFiat, in.TravelRuleThresholdFiat,
		int(in.TravelRuleWindow.Seconds()), int(in.ComplianceHoldTimeout.Seconds()),
		fields,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("corridor: upsert corridor: %w", err)
	}
	return id, nil
}

// NewProviderBinding is one provider binding passed to CreateCorridor.
type NewProviderBinding struct {
	ProviderType ProviderType
	ProviderName string
	Priority     int
}

// CreateCorridor inserts a new corridor and its provider bindings in one
// transaction — the admin API's create path. Unlike UpsertCorridor it never
// overwrites: an existing corridor with the same natural key is
// ErrAlreadyExists, so a create can't silently replace a live corridor's
// limits or compliance thresholds. Provider names are not validated here
// (this package doesn't know which adapters exist); the caller does that.
func (s *Store) CreateCorridor(ctx context.Context, in UpsertCorridorInput, bindings []NewProviderBinding) (*Corridor, []ProviderBinding, error) {
	fields := in.RequiredDestinationFields
	if fields == nil {
		fields = []string{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("corridor: create corridor: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	c, err := scanCorridor(tx.QueryRow(ctx,
		`INSERT INTO corridors (
			crypto_asset, crypto_network, fiat_currency, active,
			min_amount_fiat, max_amount_fiat, travel_rule_threshold_fiat,
			travel_rule_window_seconds, compliance_hold_timeout_seconds,
			required_destination_fields
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING `+corridorColumns,
		in.CryptoAsset, in.CryptoNetwork, in.FiatCurrency, in.Active,
		in.MinAmountFiat, in.MaxAmountFiat, in.TravelRuleThresholdFiat,
		int(in.TravelRuleWindow.Seconds()), int(in.ComplianceHoldTimeout.Seconds()),
		fields,
	))
	if isUniqueViolation(err) {
		return nil, nil, fmt.Errorf("%w: %s/%s/%s", ErrAlreadyExists, in.CryptoAsset, in.CryptoNetwork, in.FiatCurrency)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("corridor: create corridor: %w", err)
	}

	created := make([]ProviderBinding, 0, len(bindings))
	for _, b := range bindings {
		pb := ProviderBinding{
			CorridorID:   c.ID,
			ProviderType: b.ProviderType,
			ProviderName: b.ProviderName,
			Priority:     b.Priority,
			Active:       true,
			Config:       json.RawMessage(`{}`),
		}
		err := tx.QueryRow(ctx,
			`INSERT INTO corridor_providers (corridor_id, provider_type, provider_name, priority, active)
			 VALUES ($1, $2, $3, $4, true)
			 RETURNING id`,
			c.ID, string(b.ProviderType), b.ProviderName, b.Priority,
		).Scan(&pb.ID)
		if isUniqueViolation(err) {
			return nil, nil, fmt.Errorf("%w: duplicate %s provider %q", ErrAlreadyExists, b.ProviderType, b.ProviderName)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("corridor: create provider binding: %w", err)
		}
		created = append(created, pb)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("corridor: create corridor: commit: %w", err)
	}
	return c, created, nil
}

// UpdateCorridor applies apply to the current corridor row and writes the
// result back, holding a row lock for the whole read-modify-write so two
// concurrent admin edits can't silently overwrite each other. apply gets a
// copy of the current values and may mutate any field except ID and the
// natural key (CryptoAsset/CryptoNetwork/FiatCurrency), which are never
// written; returning an error aborts without writing. Returns the row as it
// was before and after the update. ErrNotFound if id doesn't exist.
func (s *Store) UpdateCorridor(ctx context.Context, id uuid.UUID, apply func(c *Corridor) error) (before, after *Corridor, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("corridor: update corridor: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	before, err = scanCorridor(tx.QueryRow(ctx,
		`SELECT `+corridorColumns+` FROM corridors WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("corridor: update corridor: %w", err)
	}

	next := *before
	next.RequiredDestinationFields = append([]string(nil), before.RequiredDestinationFields...)
	if err := apply(&next); err != nil {
		return nil, nil, err
	}
	fields := next.RequiredDestinationFields
	if fields == nil {
		fields = []string{}
	}

	after, err = scanCorridor(tx.QueryRow(ctx,
		`UPDATE corridors SET
			active = $2,
			min_amount_fiat = $3,
			max_amount_fiat = $4,
			travel_rule_threshold_fiat = $5,
			travel_rule_window_seconds = $6,
			compliance_hold_timeout_seconds = $7,
			required_destination_fields = $8,
			updated_at = now()
		 WHERE id = $1
		 RETURNING `+corridorColumns,
		id, next.Active, next.MinAmountFiat, next.MaxAmountFiat, next.TravelRuleThresholdFiat,
		int(next.TravelRuleWindow.Seconds()), int(next.ComplianceHoldTimeout.Seconds()), fields,
	))
	if err != nil {
		return nil, nil, fmt.Errorf("corridor: update corridor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("corridor: update corridor: commit: %w", err)
	}
	return before, after, nil
}

// ReplaceProviderBindings makes bindings the corridor's complete set of
// active provider bindings, in one transaction. Bindings not in the new set
// are deactivated, not deleted, so their history and config survive and
// re-adding one later reactivates the same row; bindings in the set are
// (re)activated with the given priority, keeping any existing config.
// Returns the resulting active bindings. ErrNotFound if the corridor
// doesn't exist. Provider names are validated by the caller, as with
// CreateCorridor.
func (s *Store) ReplaceProviderBindings(ctx context.Context, corridorID uuid.UUID, bindings []NewProviderBinding) ([]ProviderBinding, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("corridor: replace provider bindings: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Locking the corridor row serializes concurrent replaces for the same
	// corridor (and doubles as the existence check).
	var locked uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM corridors WHERE id = $1 FOR UPDATE`, corridorID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corridor: replace provider bindings: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE corridor_providers SET active = false WHERE corridor_id = $1`, corridorID,
	); err != nil {
		return nil, fmt.Errorf("corridor: replace provider bindings: deactivate: %w", err)
	}

	out := make([]ProviderBinding, 0, len(bindings))
	for _, b := range bindings {
		pb := ProviderBinding{CorridorID: corridorID, ProviderType: b.ProviderType, ProviderName: b.ProviderName, Priority: b.Priority, Active: true}
		err := tx.QueryRow(ctx,
			`INSERT INTO corridor_providers (corridor_id, provider_type, provider_name, priority, active)
			 VALUES ($1, $2, $3, $4, true)
			 ON CONFLICT (corridor_id, provider_type, provider_name) DO UPDATE SET
				active = true,
				priority = EXCLUDED.priority
			 RETURNING id, config`,
			corridorID, string(b.ProviderType), b.ProviderName, b.Priority,
		).Scan(&pb.ID, &pb.Config)
		if err != nil {
			return nil, fmt.Errorf("corridor: replace provider bindings: upsert %s/%s: %w", b.ProviderType, b.ProviderName, err)
		}
		out = append(out, pb)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("corridor: replace provider bindings: commit: %w", err)
	}
	return out, nil
}

// isUniqueViolation detects a Postgres unique-constraint violation (SQLSTATE
// 23505) — same helper shape internal/tenant already uses.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// UpsertProviderBinding wires an existing adapter (identified by
// providerName, which must correspond to a registered adapter in code)
// into a corridor at the given priority. This is the config-driven part of
// adding a provider — the adapter implementation itself is not.
func (s *Store) UpsertProviderBinding(ctx context.Context, corridorID uuid.UUID, providerType ProviderType, providerName string, priority int, active bool, config json.RawMessage) (uuid.UUID, error) {
	if config == nil {
		config = json.RawMessage("{}")
	}
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO corridor_providers (corridor_id, provider_type, provider_name, priority, active, config)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (corridor_id, provider_type, provider_name) DO UPDATE SET
			priority = EXCLUDED.priority,
			active = EXCLUDED.active,
			config = EXCLUDED.config
		 RETURNING id`,
		corridorID, string(providerType), providerName, priority, active, config,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("corridor: upsert provider binding: %w", err)
	}
	return id, nil
}

// ListActiveFiatCurrencies returns the distinct fiat currencies used by
// active corridors. The rate engine's background fetch job polls exactly
// this list (see ARCHITECTURE.md §7) — adding a fiat currency is a
// corridor config change, not a redeploy.
func (s *Store) ListActiveFiatCurrencies(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT fiat_currency FROM corridors WHERE active ORDER BY fiat_currency`,
	)
	if err != nil {
		return nil, fmt.Errorf("corridor: list active fiat currencies: %w", err)
	}
	defer rows.Close()

	var currencies []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("corridor: scan fiat currency: %w", err)
		}
		currencies = append(currencies, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("corridor: list active fiat currencies: %w", err)
	}
	return currencies, nil
}

// HasActiveCorridorForFiat reports whether at least one active corridor
// settles in fiatCurrency — the same set ListActiveFiatCurrencies (and so
// the rate engine's CurrentRateJob) works from. The public rate endpoint
// uses it so a currency whose corridors were all deactivated stops
// publishing a rate the job no longer refreshes.
func (s *Store) HasActiveCorridorForFiat(ctx context.Context, fiatCurrency string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM corridors WHERE active AND fiat_currency = $1)`, fiatCurrency,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("corridor: has active corridor for fiat: %w", err)
	}
	return exists, nil
}

// ListActiveProviders returns active provider bindings for a corridor,
// ordered by priority (lowest first — the settlement retry policy fails
// over down this list before retrying the one that just failed).
func (s *Store) ListActiveProviders(ctx context.Context, corridorID uuid.UUID, providerType ProviderType) ([]ProviderBinding, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, corridor_id, provider_type, provider_name, priority, active, config
		 FROM corridor_providers
		 WHERE corridor_id = $1 AND provider_type = $2 AND active
		 ORDER BY priority ASC`,
		corridorID, string(providerType),
	)
	if err != nil {
		return nil, fmt.Errorf("corridor: list active providers: %w", err)
	}
	defer rows.Close()

	var bindings []ProviderBinding
	for rows.Next() {
		var b ProviderBinding
		var pt string
		if err := rows.Scan(&b.ID, &b.CorridorID, &pt, &b.ProviderName, &b.Priority, &b.Active, &b.Config); err != nil {
			return nil, fmt.Errorf("corridor: scan provider binding: %w", err)
		}
		b.ProviderType = ProviderType(pt)
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("corridor: list active providers: %w", err)
	}
	return bindings, nil
}
