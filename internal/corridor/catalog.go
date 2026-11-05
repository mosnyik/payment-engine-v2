package corridor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The supported crypto catalog (migration 000038): which networks and
// assets corridors may use, and which networks each asset is on. Codes are
// what corridors store, so they're uppercase and never change.

var (
	// ErrInvalidCode: a code isn't 2–20 of A–Z, 0–9 or underscore.
	ErrInvalidCode = errors.New("corridor: invalid code")
	// ErrInvalidName: a name is empty.
	ErrInvalidName = errors.New("corridor: name is required")
	// ErrUnknownNetwork: an asset references a network not in the catalog.
	ErrUnknownNetwork = errors.New("corridor: unknown network")
	// ErrUnsupportedCrypto is returned by CheckCryptoSupported for an
	// asset/network pair a new corridor may not use.
	ErrUnsupportedCrypto = errors.New("corridor: unsupported crypto")
)

var codePattern = regexp.MustCompile(`^[A-Z0-9_]{2,20}$`)

// NormalizeCode uppercases and trims a catalog code and checks its shape.
func NormalizeCode(code string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(code))
	if !codePattern.MatchString(c) {
		return "", fmt.Errorf("%w: %q (want 2-20 of A-Z, 0-9, _)", ErrInvalidCode, code)
	}
	return c, nil
}

func normalizeName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", ErrInvalidName
	}
	return n, nil
}

type Network struct {
	Code      string
	Name      string
	Active    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Asset struct {
	Code      string
	Name      string
	Active    bool
	Networks  []string // network codes this asset is available on, sorted
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Networks -------------------------------------------------------------------

func (s *Store) ListNetworks(ctx context.Context) ([]Network, error) {
	rows, err := s.pool.Query(ctx, `SELECT code, name, active, created_at, updated_at FROM crypto_networks ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("corridor: list networks: %w", err)
	}
	defer rows.Close()
	var out []Network
	for rows.Next() {
		var n Network
		if err := rows.Scan(&n.Code, &n.Name, &n.Active, &n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, fmt.Errorf("corridor: scan network: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) GetNetwork(ctx context.Context, code string) (*Network, error) {
	var n Network
	err := s.pool.QueryRow(ctx, `SELECT code, name, active, created_at, updated_at FROM crypto_networks WHERE code = $1`,
		strings.ToUpper(strings.TrimSpace(code))).Scan(&n.Code, &n.Name, &n.Active, &n.CreatedAt, &n.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corridor: get network: %w", err)
	}
	return &n, nil
}

func (s *Store) CreateNetwork(ctx context.Context, code, name string, active bool) (*Network, error) {
	c, err := NormalizeCode(code)
	if err != nil {
		return nil, err
	}
	n, err := normalizeName(name)
	if err != nil {
		return nil, err
	}
	var out Network
	err = s.pool.QueryRow(ctx,
		`INSERT INTO crypto_networks (code, name, active) VALUES ($1, $2, $3)
		 RETURNING code, name, active, created_at, updated_at`, c, n, active,
	).Scan(&out.Code, &out.Name, &out.Active, &out.CreatedAt, &out.UpdatedAt)
	if isUniqueViolation(err) {
		return nil, ErrAlreadyExists
	}
	if err != nil {
		return nil, fmt.Errorf("corridor: create network: %w", err)
	}
	return &out, nil
}

// UpdateNetwork changes a network's name and/or active flag (nil leaves a
// field alone). Deactivating stops new corridors using it; existing
// corridors keep working.
func (s *Store) UpdateNetwork(ctx context.Context, code string, name *string, active *bool) (*Network, error) {
	var newName *string
	if name != nil {
		n, err := normalizeName(*name)
		if err != nil {
			return nil, err
		}
		newName = &n
	}
	var out Network
	err := s.pool.QueryRow(ctx,
		`UPDATE crypto_networks SET name = COALESCE($2, name), active = COALESCE($3, active), updated_at = now()
		 WHERE code = $1 RETURNING code, name, active, created_at, updated_at`,
		strings.ToUpper(strings.TrimSpace(code)), newName, active,
	).Scan(&out.Code, &out.Name, &out.Active, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corridor: update network: %w", err)
	}
	return &out, nil
}

// DeleteNetwork removes a network no corridor uses (and its asset
// availability rows). One a corridor still uses is ErrInUse.
func (s *Store) DeleteNetwork(ctx context.Context, code string) error {
	return s.deleteCatalogEntry(ctx, "crypto_networks", "crypto_network", code)
}

// Assets ---------------------------------------------------------------------

const assetColumns = `a.code, a.name, a.active, a.created_at, a.updated_at,
	COALESCE((SELECT array_agg(an.network_code ORDER BY an.network_code) FROM crypto_asset_networks an WHERE an.asset_code = a.code), '{}')`

func scanAsset(row pgx.Row) (*Asset, error) {
	var a Asset
	if err := row.Scan(&a.Code, &a.Name, &a.Active, &a.CreatedAt, &a.UpdatedAt, &a.Networks); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) ListAssets(ctx context.Context) ([]Asset, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+assetColumns+` FROM crypto_assets a ORDER BY a.code`)
	if err != nil {
		return nil, fmt.Errorf("corridor: list assets: %w", err)
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, fmt.Errorf("corridor: scan asset: %w", err)
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func (s *Store) GetAsset(ctx context.Context, code string) (*Asset, error) {
	return s.getAsset(ctx, s.pool, code)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) getAsset(ctx context.Context, q queryRower, code string) (*Asset, error) {
	a, err := scanAsset(q.QueryRow(ctx, `SELECT `+assetColumns+` FROM crypto_assets a WHERE a.code = $1`, strings.ToUpper(strings.TrimSpace(code))))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corridor: get asset: %w", err)
	}
	return a, nil
}

func normalizeNetworkList(networks []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(networks))
	for _, n := range networks {
		c, err := NormalizeCode(n)
		if err != nil {
			return nil, err
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

// setAssetNetworks replaces an asset's availability rows inside tx.
func setAssetNetworks(ctx context.Context, tx pgx.Tx, asset string, networks []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM crypto_asset_networks WHERE asset_code = $1`, asset); err != nil {
		return fmt.Errorf("corridor: clear asset networks: %w", err)
	}
	for _, n := range networks {
		_, err := tx.Exec(ctx, `INSERT INTO crypto_asset_networks (asset_code, network_code) VALUES ($1, $2)`, asset, n)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrUnknownNetwork, n)
		}
		if err != nil {
			return fmt.Errorf("corridor: add asset network: %w", err)
		}
	}
	return nil
}

func (s *Store) CreateAsset(ctx context.Context, code, name string, active bool, networks []string) (*Asset, error) {
	c, err := NormalizeCode(code)
	if err != nil {
		return nil, err
	}
	n, err := normalizeName(name)
	if err != nil {
		return nil, err
	}
	nets, err := normalizeNetworkList(networks)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("corridor: create asset: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO crypto_assets (code, name, active) VALUES ($1, $2, $3)`, c, n, active)
	if isUniqueViolation(err) {
		return nil, ErrAlreadyExists
	}
	if err != nil {
		return nil, fmt.Errorf("corridor: create asset: %w", err)
	}
	if err := setAssetNetworks(ctx, tx, c, nets); err != nil {
		return nil, err
	}
	a, err := s.getAsset(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("corridor: create asset: commit: %w", err)
	}
	return a, nil
}

// UpdateAsset changes an asset's name, active flag and/or the networks
// it's available on (nil leaves a field alone; networks replaces the whole
// list). Removing a network only stops new corridors using that pair.
func (s *Store) UpdateAsset(ctx context.Context, code string, name *string, active *bool, networks *[]string) (*Asset, error) {
	c := strings.ToUpper(strings.TrimSpace(code))
	var newName *string
	if name != nil {
		n, err := normalizeName(*name)
		if err != nil {
			return nil, err
		}
		newName = &n
	}
	var nets []string
	if networks != nil {
		var err error
		if nets, err = normalizeNetworkList(*networks); err != nil {
			return nil, err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("corridor: update asset: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx,
		`UPDATE crypto_assets SET name = COALESCE($2, name), active = COALESCE($3, active), updated_at = now() WHERE code = $1`,
		c, newName, active)
	if err != nil {
		return nil, fmt.Errorf("corridor: update asset: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	if networks != nil {
		if err := setAssetNetworks(ctx, tx, c, nets); err != nil {
			return nil, err
		}
	}
	a, err := s.getAsset(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("corridor: update asset: commit: %w", err)
	}
	return a, nil
}

// DeleteAsset removes an asset no corridor uses (and its availability
// rows). One a corridor still uses is ErrInUse.
func (s *Store) DeleteAsset(ctx context.Context, code string) error {
	return s.deleteCatalogEntry(ctx, "crypto_assets", "crypto_asset", code)
}

// deleteCatalogEntry deletes code from table unless a corridor's column
// still uses it (compared case-insensitively, since corridors created
// before the catalog may be in any case). The row is locked first so a
// corridor can't be validated against it mid-delete.
func (s *Store) deleteCatalogEntry(ctx context.Context, table, corridorColumn, code string) error {
	c := strings.ToUpper(strings.TrimSpace(code))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("corridor: delete %s: begin: %w", table, err)
	}
	defer tx.Rollback(ctx)

	var locked string
	err = tx.QueryRow(ctx, `SELECT code FROM `+table+` WHERE code = $1 FOR UPDATE`, c).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("corridor: delete %s: %w", table, err)
	}
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM corridors WHERE upper(`+corridorColumn+`) = $1)`, c).Scan(&used); err != nil {
		return fmt.Errorf("corridor: delete %s: check corridors: %w", table, err)
	}
	if used {
		return ErrInUse
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE code = $1`, c); err != nil {
		return fmt.Errorf("corridor: delete %s: %w", table, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("corridor: delete %s: commit: %w", table, err)
	}
	return nil
}

// CheckCryptoSupported reports whether a new corridor may use asset on
// network: both active in the catalog, and the asset available on that
// network. A catalog entry deleted in the instant between this check and
// the corridor insert can't orphan anything that matters: the corridor
// then simply uses a code the catalog no longer lists.
func (s *Store) CheckCryptoSupported(ctx context.Context, asset, network string) error {
	a, n := strings.ToUpper(strings.TrimSpace(asset)), strings.ToUpper(strings.TrimSpace(network))
	var assetActive, networkActive, available *bool
	err := s.pool.QueryRow(ctx,
		`SELECT (SELECT active FROM crypto_assets WHERE code = $1),
		        (SELECT active FROM crypto_networks WHERE code = $2),
		        EXISTS (SELECT 1 FROM crypto_asset_networks WHERE asset_code = $1 AND network_code = $2)`,
		a, n,
	).Scan(&assetActive, &networkActive, &available)
	if err != nil {
		return fmt.Errorf("corridor: check crypto: %w", err)
	}
	switch {
	case assetActive == nil:
		return fmt.Errorf("%w: asset %s is not in the catalog", ErrUnsupportedCrypto, a)
	case !*assetActive:
		return fmt.Errorf("%w: asset %s is deactivated", ErrUnsupportedCrypto, a)
	case networkActive == nil:
		return fmt.Errorf("%w: network %s is not in the catalog", ErrUnsupportedCrypto, n)
	case !*networkActive:
		return fmt.Errorf("%w: network %s is deactivated", ErrUnsupportedCrypto, n)
	case available == nil || !*available:
		return fmt.Errorf("%w: %s is not available on %s", ErrUnsupportedCrypto, a, n)
	}
	return nil
}

// EnsureCrypto adds asset, network and their pairing to the catalog if
// missing, leaving existing entries alone. For config-driven seeding
// (sandbox corridors), which must not fail on a fresh database.
func (s *Store) EnsureCrypto(ctx context.Context, asset, network string) error {
	a, err := NormalizeCode(asset)
	if err != nil {
		return err
	}
	n, err := NormalizeCode(network)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`WITH na AS (INSERT INTO crypto_networks (code, name) VALUES ($2, $2) ON CONFLICT DO NOTHING),
		      aa AS (INSERT INTO crypto_assets (code, name) VALUES ($1, $1) ON CONFLICT DO NOTHING)
		 SELECT 1`, a, n)
	if err != nil {
		return fmt.Errorf("corridor: ensure crypto: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO crypto_asset_networks (asset_code, network_code) VALUES ($1, $2) ON CONFLICT DO NOTHING`, a, n); err != nil {
		return fmt.Errorf("corridor: ensure crypto pair: %w", err)
	}
	return nil
}
