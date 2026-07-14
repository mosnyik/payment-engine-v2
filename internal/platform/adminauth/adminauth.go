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

const sessionTTL = 12 * time.Hour

var (
	ErrInvalidCredentials = errors.New("adminauth: invalid email or password")
	ErrInvalidSession     = errors.New("adminauth: invalid or expired session")
)

type Store struct {
	pool *db.Pool
}

func New(pool *db.Pool) *Store {
	return &Store{pool: pool}
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
		tokenHash, id, time.Now().Add(sessionTTL),
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
