// Package tenant is the bank/fintech account that integrates with the
// rail: profile, fee schedule, corridor entitlements, and credential
// storage. Implements gateway.CredentialLookup so the tenant-facing API
// gateway can authenticate requests without depending on this package
// directly — only on the interface it satisfies.
package tenant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	pcrypto "github.com/sirfi/payment-engine-v2/internal/platform/crypto"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
	"github.com/sirfi/payment-engine-v2/internal/platform/webhookurl"
)

// Compile-time proof that Store satisfies gateway.CredentialLookup —
// catches interface drift immediately rather than at first wiring.
var _ gateway.CredentialLookup = (*Store)(nil)

type Status string

const (
	StatusPendingKYB Status = "pending_kyb"
	StatusActive     Status = "active"
	StatusSuspended  Status = "suspended"
)

var (
	ErrNotFound                = errors.New("tenant: not found")
	ErrInvalidStatusTransition = errors.New("tenant: invalid status transition")
	ErrInvalidWebhookURL       = errors.New("tenant: invalid webhook url")
)

type Tenant struct {
	ID         uuid.UUID
	Name       string
	Status     Status
	FeeBps     int
	WebhookURL *string
	CreatedAt  time.Time
}

type Store struct {
	pool   *db.Pool
	crypto *pcrypto.AESGCM
}

// New builds a Store. encryptionKey is config.TenantSecretEncryptionKey —
// see internal/platform/crypto for what it protects and its limitations.
func New(pool *db.Pool, encryptionKey []byte) (*Store, error) {
	c, err := pcrypto.NewAESGCM(encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("tenant: %w", err)
	}
	return &Store{pool: pool, crypto: c}, nil
}

func (s *Store) CreateTenant(ctx context.Context, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ($1) RETURNING id`,
		name,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant: create tenant: %w", err)
	}
	return id, nil
}

func (s *Store) GetTenant(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	var t Tenant
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, status, fee_bps, webhook_url, created_at FROM tenants WHERE id = $1`,
		id,
	).Scan(&t.ID, &t.Name, &status, &t.FeeBps, &t.WebhookURL, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenant: get tenant: %w", err)
	}
	t.Status = Status(status)
	return &t, nil
}

// ApproveKYB activates a tenant. Only valid from pending_kyb — the
// compare-and-set (`WHERE status = 'pending_kyb'`) means a concurrent or
// repeated approval can't double-apply, consistent with every other status
// transition in this codebase.
func (s *Store) ApproveKYB(ctx context.Context, tenantID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET status = $2, updated_at = now() WHERE id = $1 AND status = $3`,
		tenantID, string(StatusActive), string(StatusPendingKYB),
	)
	if err != nil {
		return fmt.Errorf("tenant: approve kyb: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInvalidStatusTransition
	}
	return nil
}

// Suspend deactivates a tenant — their existing API keys immediately stop
// authenticating (see LookupHMACSecret), regardless of key-level active state.
func (s *Store) Suspend(ctx context.Context, tenantID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET status = $2, updated_at = now() WHERE id = $1 AND status != $2`,
		tenantID, string(StatusSuspended),
	)
	if err != nil {
		return fmt.Errorf("tenant: suspend: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInvalidStatusTransition
	}
	return nil
}

// IssueAPIKey generates a new API key + HMAC secret for an active tenant.
// The raw secret is returned once and never stored — only its encrypted
// form is persisted. Issuing credentials for a non-active tenant is
// refused: nothing is issued before KYB clears (ARCHITECTURE.md §5).
func (s *Store) IssueAPIKey(ctx context.Context, tenantID uuid.UUID) (apiKey, hmacSecret string, err error) {
	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return "", "", err
	}
	if t.Status != StatusActive {
		return "", "", fmt.Errorf("tenant: cannot issue credentials for tenant in status %q", t.Status)
	}

	apiKeySuffix, err := randomHex(16)
	if err != nil {
		return "", "", err
	}
	apiKey = "pk_" + apiKeySuffix

	hmacSecret, err = randomHex(32)
	if err != nil {
		return "", "", err
	}

	encrypted, err := s.crypto.Encrypt(hmacSecret)
	if err != nil {
		return "", "", fmt.Errorf("tenant: encrypt hmac secret: %w", err)
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO tenant_api_keys (tenant_id, api_key, hmac_secret_encrypted) VALUES ($1, $2, $3)`,
		tenantID, apiKey, encrypted,
	)
	if err != nil {
		return "", "", fmt.Errorf("tenant: issue api key: %w", err)
	}

	return apiKey, hmacSecret, nil
}

// LookupHMACSecret implements gateway.CredentialLookup. A revoked key or a
// non-active tenant both resolve to "unknown" (ok=false) — from the
// gateway's perspective there is no meaningful difference between "this key
// never existed" and "this key must no longer authenticate".
func (s *Store) LookupHMACSecret(ctx context.Context, apiKey string) (string, uuid.UUID, bool, error) {
	var tenantID uuid.UUID
	var encryptedSecret string
	var keyActive bool
	var tenantStatus string
	err := s.pool.QueryRow(ctx,
		`SELECT k.tenant_id, k.hmac_secret_encrypted, k.active, t.status
		 FROM tenant_api_keys k
		 JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.api_key = $1`,
		apiKey,
	).Scan(&tenantID, &encryptedSecret, &keyActive, &tenantStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, false, nil
	}
	if err != nil {
		return "", uuid.Nil, false, fmt.Errorf("tenant: lookup hmac secret: %w", err)
	}
	if !keyActive || Status(tenantStatus) != StatusActive {
		return "", uuid.Nil, false, nil
	}

	secret, err := s.crypto.Decrypt(encryptedSecret)
	if err != nil {
		return "", uuid.Nil, false, fmt.Errorf("tenant: decrypt hmac secret: %w", err)
	}
	return secret, tenantID, true, nil
}

// SetCorridorEntitlement grants or revokes a tenant's access to a corridor,
// with an optional per-corridor fee override.
func (s *Store) SetCorridorEntitlement(ctx context.Context, tenantID, corridorID uuid.UUID, active bool, feeBpsOverride *int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tenant_corridor_entitlements (tenant_id, corridor_id, active, fee_bps_override)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, corridor_id) DO UPDATE SET
			active = EXCLUDED.active,
			fee_bps_override = EXCLUDED.fee_bps_override`,
		tenantID, corridorID, active, feeBpsOverride,
	)
	if err != nil {
		return fmt.Errorf("tenant: set corridor entitlement: %w", err)
	}
	return nil
}

// CheckEntitlement reports whether a tenant may use a corridor. No row at
// all (never granted) and an explicitly deactivated row both mean false —
// callers don't need to distinguish "never entitled" from "revoked".
func (s *Store) CheckEntitlement(ctx context.Context, tenantID, corridorID uuid.UUID) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx,
		`SELECT active FROM tenant_corridor_entitlements WHERE tenant_id = $1 AND corridor_id = $2`,
		tenantID, corridorID,
	).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("tenant: check entitlement: %w", err)
	}
	return active, nil
}

// SetWebhookURL validates and stores a tenant's outbound webhook endpoint.
// This validates at registration time only — re-checking at delivery time
// (to guard against DNS rebinding between registration and send) is a
// Phase 7 (notification) concern, not implemented here.
func (s *Store) SetWebhookURL(ctx context.Context, tenantID uuid.UUID, webhookURL string) error {
	if err := ValidateWebhookURL(webhookURL); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidWebhookURL, err)
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE tenants SET webhook_url = $2, updated_at = now() WHERE id = $1`,
		tenantID, webhookURL,
	)
	if err != nil {
		return fmt.Errorf("tenant: set webhook url: %w", err)
	}
	return nil
}

// ValidateWebhookURL rejects anything unsafe to send a webhook to — see
// platform/webhookurl.Validate (shared with treasury's tenant-notification
// sender, so the SSRF check exists in exactly one place). This is the
// direct fix for the v1 audit's SSRF-via-webhook-URL finding, which only
// checked URL syntax and accepted internal addresses outright.
func ValidateWebhookURL(rawURL string) error {
	return webhookurl.Validate(rawURL)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("tenant: generate random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
