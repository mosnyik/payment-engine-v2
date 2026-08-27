// Package adminauth is the credential space for internal staff (KYB review,
// compliance holds, corridor config, manual settlement retry) — separate
// from the tenant-facing gateway. Per-admin credentials, sessions looked up
// by token hash (never a shared secret compared in application code), and
// every sensitive action audit-logged.
package adminauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

var (
	ErrInvalidCredentials = errors.New("adminauth: invalid email or password")
	ErrInvalidSession     = errors.New("adminauth: invalid or expired session")
)

type Store struct {
	pool       *db.Pool
	sessionTTL time.Duration
}

// New builds a Store. sessionTTL is an operational setting (config.AdminSessionTTL),
// not a compiled-in constant, so it's tunable per environment without a redeploy.
func New(pool *db.Pool, sessionTTL time.Duration) *Store {
	return &Store{pool: pool, sessionTTL: sessionTTL}
}

// CreateAdmin creates a new admin user with a bcrypt-hashed password.
// Intended for a one-off provisioning script/CLI, not a public endpoint —
// there is no self-service admin signup.
func (s *Store) CreateAdmin(ctx context.Context, email, password string) (uuid.UUID, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return uuid.Nil, fmt.Errorf("adminauth: hash password: %w", err)
	}

	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO admin_users (email, password_hash) VALUES ($1, $2) RETURNING id`,
		email, string(hash),
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("adminauth: create admin: %w", err)
	}
	return id, nil
}

// Login verifies email+password and issues a new opaque session token,
// valid for sessionTTL. The raw token is returned once and never stored —
// only its SHA-256 hash is persisted, so a database read alone can't yield
// a usable credential.
func (s *Store) Login(ctx context.Context, email, password string) (string, error) {
	var id uuid.UUID
	var passwordHash string
	var disabledAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, password_hash, disabled_at FROM admin_users WHERE email = $1`,
		email,
	).Scan(&id, &passwordHash, &disabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Run the bcrypt comparison anyway against a dummy hash so a
		// nonexistent-email response takes the same time as a
		// wrong-password one — avoids the account-enumeration timing gap.
		_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
		return "", ErrInvalidCredentials
	}
	if err != nil {
		return "", fmt.Errorf("adminauth: lookup admin: %w", err)
	}
	if disabledAt != nil {
		return "", ErrInvalidCredentials
	}

	if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); err != nil {
		return "", ErrInvalidCredentials
	}

	token, tokenHash, err := newToken()
	if err != nil {
		return "", err
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO admin_sessions (token_hash, admin_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, id, time.Now().Add(s.sessionTTL),
	)
	if err != nil {
		return "", fmt.Errorf("adminauth: create session: %w", err)
	}

	return token, nil
}

// Authenticate resolves a bearer token to an admin ID. The token is looked
// up by its hash — there is no secret-comparison step at all, which
// sidesteps timing-attack concerns structurally rather than relying on a
// constant-time compare of a single shared value (the v1 audit finding
// this replaces).
func (s *Store) Authenticate(ctx context.Context, token string) (uuid.UUID, error) {
	tokenHash := hashToken(token)

	var adminID uuid.UUID
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT admin_id, expires_at FROM admin_sessions WHERE token_hash = $1`,
		tokenHash,
	).Scan(&adminID, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrInvalidSession
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("adminauth: lookup session: %w", err)
	}
	if time.Now().After(expiresAt) {
		return uuid.Nil, ErrInvalidSession
	}

	return adminID, nil
}

// LoginWithOIDC resolves an OIDC-authenticated identity (subject/email
// claims from a verified ID token, see oidc.go's Exchange) to an admin
// session. Invite-only: this never creates an admin_users row — a row must
// already exist (provisioned via adminctl) before its owner can SSO in.
//
// Two lookup paths: an admin who has already logged in via SSO once is
// found by oidc_subject directly (the stable, spec-correct binding); an
// admin who hasn't yet is found by email and bound to this subject on this
// call, so a row provisioned with just a password can migrate to SSO the
// first time its owner uses it — email is only ever trusted for this
// one-time binding, never as the ongoing identity key.
func (s *Store) LoginWithOIDC(ctx context.Context, subject, email string) (string, error) {
	id, err := s.resolveOIDCAdmin(ctx, subject, email)
	if err != nil {
		return "", err
	}

	token, tokenHash, err := newToken()
	if err != nil {
		return "", err
	}

	_, err = s.pool.Exec(ctx,
		`INSERT INTO admin_sessions (token_hash, admin_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, id, time.Now().Add(s.sessionTTL),
	)
	if err != nil {
		return "", fmt.Errorf("adminauth: create session: %w", err)
	}

	return token, nil
}

func (s *Store) resolveOIDCAdmin(ctx context.Context, subject, email string) (uuid.UUID, error) {
	var id uuid.UUID
	var disabledAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, disabled_at FROM admin_users WHERE oidc_subject = $1`,
		subject,
	).Scan(&id, &disabledAt)
	if err == nil {
		if disabledAt != nil {
			return uuid.Nil, ErrInvalidCredentials
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("adminauth: lookup admin by oidc subject: %w", err)
	}

	// No row bound to this subject yet — fall back to a one-time
	// email-based bind, only against a row that isn't already bound to a
	// *different* subject (the UNIQUE constraint on oidc_subject also
	// guards this, but checking IS NULL here keeps the intent explicit and
	// avoids a constraint-violation error path on a should-be-rare race).
	err = s.pool.QueryRow(ctx,
		`SELECT id, disabled_at FROM admin_users WHERE email = $1 AND oidc_subject IS NULL`,
		email,
	).Scan(&id, &disabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrInvalidCredentials
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("adminauth: lookup admin by email: %w", err)
	}
	if disabledAt != nil {
		return uuid.Nil, ErrInvalidCredentials
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE admin_users SET oidc_subject = $1 WHERE id = $2 AND oidc_subject IS NULL`,
		subject, id,
	); err != nil {
		return uuid.Nil, fmt.Errorf("adminauth: bind oidc subject: %w", err)
	}

	return id, nil
}

// LogAction records an audited admin action. metadata is marshaled to JSON;
// pass nil if there's nothing beyond action/target worth recording.
func (s *Store) LogAction(ctx context.Context, adminID uuid.UUID, action, target string, metadata any) error {
	var metadataJSON []byte
	if metadata != nil {
		var err error
		metadataJSON, err = json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("adminauth: marshal audit metadata: %w", err)
		}
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO admin_audit_log (admin_id, action, target, metadata) VALUES ($1, $2, $3, $4)`,
		adminID, action, target, metadataJSON,
	)
	if err != nil {
		return fmt.Errorf("adminauth: log action: %w", err)
	}
	return nil
}

// AuditLogEntry is one admin_audit_log row — the human-readable "who
// approved what" trail LogAction writes to.
type AuditLogEntry struct {
	ID        uuid.UUID
	AdminID   uuid.UUID
	Action    string
	Target    *string
	Metadata  json.RawMessage
	CreatedAt time.Time
}

// ListAuditLog is the admin browse surface (Phase 11) over admin_audit_log —
// newest first, optionally restricted to one admin's own actions. limit<=0
// means unbounded.
func (s *Store) ListAuditLog(ctx context.Context, adminID *uuid.UUID, limit, offset int) ([]AuditLogEntry, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_log WHERE ($1::uuid IS NULL OR admin_id = $1)`,
		adminID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("adminauth: list audit log: count: %w", err)
	}

	query := `SELECT id, admin_id, action, target, metadata, created_at FROM admin_audit_log
		WHERE ($1::uuid IS NULL OR admin_id = $1) ORDER BY created_at DESC`
	args := []any{adminID}
	if limit > 0 {
		query += ` LIMIT $2 OFFSET $3`
		args = append(args, limit, offset)
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("adminauth: list audit log: %w", err)
	}
	defer rows.Close()

	var out []AuditLogEntry
	for rows.Next() {
		var e AuditLogEntry
		if err := rows.Scan(&e.ID, &e.AdminID, &e.Action, &e.Target, &e.Metadata, &e.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("adminauth: list audit log: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("adminauth: list audit log: %w", err)
	}
	return out, total, nil
}

func newToken() (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("adminauth: generate token: %w", err)
	}
	raw = hex.EncodeToString(buf)
	return raw, hashToken(raw), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// dummyHash is a valid bcrypt hash with no corresponding real password,
// used only to burn equivalent time on a nonexistent-email login attempt.
const dummyHash = "$2a$10$CwTycUXWue0Thq9StjUM0uJ8Bpn.jhpxYAv8/lE7RNi6UDgYq0M0O"
