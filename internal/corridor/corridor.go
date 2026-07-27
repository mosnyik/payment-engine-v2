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
