// Package tenant is the bank/fintech account that integrates with the
// rail: profile, fee schedule, corridor entitlements, and credential
// storage. Implements gateway.CredentialLookup so the tenant-facing API
// gateway can authenticate requests without depending on this package
// directly — only on the interface it satisfies.
package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	pcrypto "github.com/sirfi/payment-engine-v2/internal/platform/crypto"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/gateway"
	"github.com/sirfi/payment-engine-v2/internal/platform/webhookurl"
)

// PortalMagicLinkTTL / PortalSessionTTL are fixed design constants, not ops
// knobs — same convention session.SessionTTL/rate.slippageBuffer already
// use for timers that don't need per-environment tuning.
const (
	PortalMagicLinkTTL = 15 * time.Minute
	PortalSessionTTL   = 12 * time.Hour
)

// EmailSender is the narrow capability RequestMagicLink needs to deliver a
// login link. Structurally identical to notification.EmailProvider (Name
// omitted — not needed here), so cmd/server can wire that already-
// configured vendor adapter straight into SetEmailSender without this
// package ever importing internal/notification directly, which would
// invert the existing dependency direction (notification depends on
// tenant via TenantWebhookLookup, never the reverse).
type EmailSender interface {
	IsEnabled() bool
	Send(ctx context.Context, to, subject, body string) error
}

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
	ErrNotFound                 = errors.New("tenant: not found")
	ErrInvalidStatusTransition  = errors.New("tenant: invalid status transition")
	ErrInvalidWebhookURL        = errors.New("tenant: invalid webhook url")
	ErrEmailTaken               = errors.New("tenant: email already registered")
	ErrInvalidMagicLink         = errors.New("tenant: invalid or expired magic link")
	ErrPortalSessionInvalid     = errors.New("tenant: invalid or expired session")
	ErrEmailDeliveryUnavailable = errors.New("tenant: email delivery not configured")
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
	pool          *db.Pool
	crypto        *pcrypto.AESGCM
	emailSender   EmailSender
	portalBaseURL string
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

// SetEmailSender wires the magic-link delivery mechanism — optional-
// dependency convention (nil-safe field + setter) this codebase already
// uses for eventbus wiring (e.g. treasury.SetEventBus), so every existing
// tenant.New call site, none of which are concerned with portal auth,
// stays unaffected.
func (s *Store) SetEmailSender(sender EmailSender) {
	s.emailSender = sender
}

// SetPortalBaseURL wires the tenant portal frontend's origin (e.g.
// https://portal.2settle.io) — same optional-dependency setter convention
// as SetEmailSender. When unset, RequestMagicLink emails the bare token
// instead of a clickable link, so this stays safe to leave unconfigured
// (config.PortalBaseURL defaults to "").
func (s *Store) SetPortalBaseURL(baseURL string) {
	s.portalBaseURL = baseURL
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

// RegisterTenant creates a new self-service tenant — status pending_kyb,
// same starting state CreateTenant already uses — with a contact email for
// portal login. Passwordless: see RequestMagicLink/VerifyMagicLink for how
// that email is turned into a session.
func (s *Store) RegisterTenant(ctx context.Context, name, email string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tenants (name, contact_email) VALUES ($1, $2) RETURNING id`,
		name, email,
	).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return uuid.Nil, ErrEmailTaken
		}
		return uuid.Nil, fmt.Errorf("tenant: register tenant: %w", err)
	}
	return id, nil
}

// RequestMagicLink issues a single-use, time-limited login token for the
// tenant registered under email and, if an EmailSender is configured,
// delivers it. Returns ErrNotFound if no tenant has this email — the HTTP
// handler is responsible for hiding that distinction behind a uniform
// response so this can't be used to enumerate registered emails (same
// split of responsibility sessionHandlers.getSession already uses: the
// Store tells the truth, the handler decides what to reveal).
func (s *Store) RequestMagicLink(ctx context.Context, email string) error {
	var tenantID uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM tenants WHERE contact_email = $1`, email).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("tenant: lookup by email: %w", err)
	}

	token, tokenHash, err := newPortalToken()
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO tenant_magic_links (token_hash, tenant_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, tenantID, time.Now().Add(PortalMagicLinkTTL),
	)
	if err != nil {
		return fmt.Errorf("tenant: store magic link: %w", err)
	}

	if s.emailSender == nil || !s.emailSender.IsEnabled() {
		return ErrEmailDeliveryUnavailable
	}
	subject := "Your 2Settle dashboard login link"
	body := magicLinkEmailBody(token, s.portalBaseURL)
	if err := s.emailSender.Send(ctx, email, subject, body); err != nil {
		return fmt.Errorf("tenant: send magic link email: %w", err)
	}
	return nil
}

// magicLinkEmailBody renders a real "{portalBaseURL}/verify?token=..." link
// when portalBaseURL is configured (see SetPortalBaseURL), so the email
// contains something clickable instead of a bare token the recipient would
// have to copy into a request body by hand. Falls back to the bare token
// when no frontend origin is configured, so a deployment that hasn't set
// PORTAL_BASE_URL yet still sends something usable, just not clickable.
func magicLinkEmailBody(token, portalBaseURL string) string {
	link := token
	if portalBaseURL != "" {
		link = fmt.Sprintf("%s/v2/portal/verify?token=%s", strings.TrimRight(portalBaseURL, "/"), url.QueryEscape(token))
	}
	expiryMinutes := int(PortalMagicLinkTTL.Minutes())
	return fmt.Sprintf("Use this link to sign in: %s\n\nThis link expires in %d minutes and can only be used once.", link, expiryMinutes)
}

// VerifyMagicLink redeems a single-use magic-link token and issues a real
// portal session token in its place — CAS-consumed (UPDATE ... WHERE
// consumed_at IS NULL) the same way every other state transition in this
// codebase claims a one-time action, so a token can't be redeemed twice
// even under concurrent requests.
func (s *Store) VerifyMagicLink(ctx context.Context, token string) (sessionToken string, tenantID uuid.UUID, err error) {
	tokenHash := hashPortalToken(token)

	var expiresAt time.Time
	var consumedAt *time.Time
	err = s.pool.QueryRow(ctx,
		`SELECT tenant_id, expires_at, consumed_at FROM tenant_magic_links WHERE token_hash = $1`,
		tokenHash,
	).Scan(&tenantID, &expiresAt, &consumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, ErrInvalidMagicLink
	}
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("tenant: lookup magic link: %w", err)
	}
	if consumedAt != nil || time.Now().After(expiresAt) {
		return "", uuid.Nil, ErrInvalidMagicLink
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE tenant_magic_links SET consumed_at = now() WHERE token_hash = $1 AND consumed_at IS NULL`,
		tokenHash,
	)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("tenant: consume magic link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Consumed by a concurrent request between the SELECT and this
		// UPDATE — treated identically to "already invalid," same race
		// window every other CAS transition in this codebase accepts.
		return "", uuid.Nil, ErrInvalidMagicLink
	}

	sessionToken, sessionHash, err := newPortalToken()
	if err != nil {
		return "", uuid.Nil, err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO tenant_sessions (token_hash, tenant_id, expires_at) VALUES ($1, $2, $3)`,
		sessionHash, tenantID, time.Now().Add(PortalSessionTTL),
	)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("tenant: create session: %w", err)
	}
	return sessionToken, tenantID, nil
}

// AuthenticateSession resolves a portal bearer token to a tenant ID —
// mirrors adminauth.Store.Authenticate exactly: looked up by the token's
// hash, no secret-comparison step at all, sidestepping timing-attack
// concerns structurally.
func (s *Store) AuthenticateSession(ctx context.Context, token string) (uuid.UUID, error) {
	tokenHash := hashPortalToken(token)

	var tenantID uuid.UUID
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT tenant_id, expires_at FROM tenant_sessions WHERE token_hash = $1`,
		tokenHash,
	).Scan(&tenantID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrPortalSessionInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant: lookup session: %w", err)
	}
	if time.Now().After(expiresAt) {
		return uuid.Nil, ErrPortalSessionInvalid
	}
	return tenantID, nil
}

// APIKeySummary is the self-service view of an issued API key — never
// includes the HMAC secret, which is shown once at issuance and never
// again.
type APIKeySummary struct {
	ID        uuid.UUID
	APIKey    string
	Active    bool
	CreatedAt time.Time
	RevokedAt *time.Time
}

// ListAPIKeys returns a tenant's own API keys, newest first.
func (s *Store) ListAPIKeys(ctx context.Context, tenantID uuid.UUID) ([]APIKeySummary, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, api_key, active, created_at, revoked_at FROM tenant_api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("tenant: list api keys: %w", err)
	}
	defer rows.Close()

	var out []APIKeySummary
	for rows.Next() {
		var k APIKeySummary
		if err := rows.Scan(&k.ID, &k.APIKey, &k.Active, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("tenant: list api keys: scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey deactivates one of a tenant's own API keys. Scoped by
// tenant_id in the same query as the active check — a key ID that exists
// but belongs to a different tenant is indistinguishable from one that
// doesn't exist at all, same reasoning LookupHMACSecret's doc comment
// already gives for not telling those cases apart.
func (s *Store) RevokeAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenant_api_keys SET active = false, revoked_at = now() WHERE id = $1 AND tenant_id = $2 AND active = true`,
		keyID, tenantID,
	)
	if err != nil {
		return fmt.Errorf("tenant: revoke api key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
func (s *Store) LookupHMACSecret(ctx context.Context, apiKey string) (string, uuid.UUID, uuid.UUID, bool, error) {
	var keyID uuid.UUID
	var tenantID uuid.UUID
	var encryptedSecret string
	var keyActive bool
	var tenantStatus string
	err := s.pool.QueryRow(ctx,
		`SELECT k.id, k.tenant_id, k.hmac_secret_encrypted, k.active, t.status
		 FROM tenant_api_keys k
		 JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.api_key = $1`,
		apiKey,
	).Scan(&keyID, &tenantID, &encryptedSecret, &keyActive, &tenantStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", uuid.Nil, uuid.Nil, false, nil
	}
	if err != nil {
		return "", uuid.Nil, uuid.Nil, false, fmt.Errorf("tenant: lookup hmac secret: %w", err)
	}
	if !keyActive || Status(tenantStatus) != StatusActive {
		return "", uuid.Nil, uuid.Nil, false, nil
	}

	secret, err := s.crypto.Decrypt(encryptedSecret)
	if err != nil {
		return "", uuid.Nil, uuid.Nil, false, fmt.Errorf("tenant: decrypt hmac secret: %w", err)
	}
	return secret, tenantID, keyID, true, nil
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

// EffectiveFeeBps resolves the fee (in basis points) settlement should apply
// for a tenant on a corridor: the corridor entitlement's fee_bps_override if
// set, else the tenant's flat FeeBps. Requires an entitlement row to exist
// (i.e. CheckEntitlement must already have passed) — a tenant with no
// entitlement on this corridor has nothing to fall back to but its own
// FeeBps, so this deliberately doesn't try to guess a default from a
// missing row the way CheckEntitlement's "no row = false" does.
func (s *Store) EffectiveFeeBps(ctx context.Context, tenantID, corridorID uuid.UUID) (int, error) {
	var feeBps int
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(e.fee_bps_override, t.fee_bps)
		 FROM tenants t
		 JOIN tenant_corridor_entitlements e ON e.tenant_id = t.id AND e.corridor_id = $2
		 WHERE t.id = $1`,
		tenantID, corridorID,
	).Scan(&feeBps)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("tenant: effective fee bps: %w", ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("tenant: effective fee bps: %w", err)
	}
	return feeBps, nil
}

// SetWebhookURL validates and stores a tenant's outbound webhook endpoint.
// Validates at registration time; callers that actually deliver to this
// URL (e.g. treasury's tenant-notification sender) re-validate again
// immediately before each send via platform/webhookurl.Validate, guarding
// against DNS rebinding between registration and send.
//
// The first call for a tenant also generates a webhook signing secret
// (returned once, like IssueAPIKey's HMAC secret — never shown again) so
// the tenant can verify a delivered webhook actually came from this
// system. A later call to change the URL keeps the existing secret;
// signingSecret is empty in that case.
func (s *Store) SetWebhookURL(ctx context.Context, tenantID uuid.UUID, webhookURL string) (signingSecret string, err error) {
	if err := ValidateWebhookURL(webhookURL); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidWebhookURL, err)
	}

	var existingSecret *string
	err = s.pool.QueryRow(ctx, `SELECT webhook_signing_secret_encrypted FROM tenants WHERE id = $1`, tenantID).Scan(&existingSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("tenant: check webhook signing secret: %w", err)
	}

	var encryptedSecret *string
	if existingSecret == nil {
		signingSecret, err = randomHex(32)
		if err != nil {
			return "", err
		}
		encrypted, err := s.crypto.Encrypt(signingSecret)
		if err != nil {
			return "", fmt.Errorf("tenant: encrypt webhook signing secret: %w", err)
		}
		encryptedSecret = &encrypted
	} else {
		encryptedSecret = existingSecret
	}

	_, err = s.pool.Exec(ctx,
		`UPDATE tenants SET webhook_url = $2, webhook_signing_secret_encrypted = $3, updated_at = now() WHERE id = $1`,
		tenantID, webhookURL, encryptedSecret,
	)
	if err != nil {
		return "", fmt.Errorf("tenant: set webhook url: %w", err)
	}
	return signingSecret, nil
}

// WebhookConfig returns a tenant's registered webhook URL and (decrypted)
// signing secret — the lookup treasury's TenantWebhookLookup interface
// calls through, so treasury never imports this package directly.
func (s *Store) WebhookConfig(ctx context.Context, tenantID uuid.UUID) (webhookURL, signingSecret string, ok bool, err error) {
	var url, encryptedSecret *string
	err = s.pool.QueryRow(ctx,
		`SELECT webhook_url, webhook_signing_secret_encrypted FROM tenants WHERE id = $1`,
		tenantID,
	).Scan(&url, &encryptedSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("tenant: get webhook config: %w", err)
	}
	if url == nil || encryptedSecret == nil {
		return "", "", false, nil
	}
	secret, err := s.crypto.Decrypt(*encryptedSecret)
	if err != nil {
		return "", "", false, fmt.Errorf("tenant: decrypt webhook signing secret: %w", err)
	}
	return *url, secret, true, nil
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

// newPortalToken/hashPortalToken back both magic-link and session tokens —
// same 32-random-bytes-hex/SHA-256-hash shape adminauth.newToken/hashToken
// already use, duplicated locally per this codebase's existing convention
// of each package owning its own small helpers rather than sharing one.
func newPortalToken() (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("tenant: generate token: %w", err)
	}
	raw = hex.EncodeToString(buf)
	return raw, hashPortalToken(raw), nil
}

func hashPortalToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// isUniqueViolation detects a Postgres unique-constraint violation (SQLSTATE
// 23505) — same helper shape internal/treasury/hdwallet.go already
// establishes for the identical need.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
