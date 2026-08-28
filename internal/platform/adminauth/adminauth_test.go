package adminauth_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"github.com/sirfi/payment-engine-v2/internal/platform/adminauth"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	_ = godotenv.Load("../../../.env")

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping integration test")
	}

	pool, err := db.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestLoginAuthenticateAndAudit(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	store := adminauth.New(pool, 12*time.Hour)

	email := fmt.Sprintf("ops-%s@sirfi.test", uuid.New())
	password := "correct-horse-battery-staple"

	adminID, err := store.CreateAdmin(ctx, email, password)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}

	token, err := store.Login(ctx, email, password)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if token == "" {
		t.Fatal("expected a non-empty token")
	}

	gotID, err := store.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if gotID != adminID {
		t.Fatalf("expected admin %s, got %s", adminID, gotID)
	}

	if err := store.LogAction(ctx, adminID, "kyb.approved", "tenant:abc123", map[string]string{"reason": "docs verified"}); err != nil {
		t.Fatalf("log action: %v", err)
	}

	var loggedAction string
	err = pool.QueryRow(ctx, `SELECT action FROM admin_audit_log WHERE admin_id = $1`, adminID).Scan(&loggedAction)
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if loggedAction != "kyb.approved" {
		t.Fatalf("expected logged action 'kyb.approved', got %q", loggedAction)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	store := adminauth.New(pool, 12*time.Hour)

	email := fmt.Sprintf("ops-%s@sirfi.test", uuid.New())
	if _, err := store.CreateAdmin(ctx, email, "the-real-password"); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	if _, err := store.Login(ctx, email, "wrong-password"); err != adminauth.ErrInvalidCredentials {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLoginUnknownEmail(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	store := adminauth.New(pool, 12*time.Hour)

	if _, err := store.Login(ctx, "nobody@sirfi.test", "whatever"); err != adminauth.ErrInvalidCredentials {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestAuthenticateInvalidToken(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	store := adminauth.New(pool, 12*time.Hour)

	if _, err := store.Authenticate(ctx, "not-a-real-token"); err != adminauth.ErrInvalidSession {
		t.Fatalf("expected ErrInvalidSession, got %v", err)
	}
}

func TestLoginWithOIDCUnprovisionedEmailRejected(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	store := adminauth.New(pool, 12*time.Hour)

	email := fmt.Sprintf("nobody-%s@sirfi.test", uuid.New())
	if _, err := store.LoginWithOIDC(ctx, "sub-"+uuid.New().String(), email); err != adminauth.ErrInvalidCredentials {
		t.Fatalf("expected ErrInvalidCredentials for an unprovisioned email (invite-only), got %v", err)
	}
}

func TestLoginWithOIDCBindsExistingAdminByEmailThenUsesSubject(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	store := adminauth.New(pool, 12*time.Hour)

	email := fmt.Sprintf("ops-%s@sirfi.test", uuid.New())
	adminID, err := store.CreateAdmin(ctx, email, "some-password-never-used")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	subject := "sub-" + uuid.New().String()

	// First OIDC login: no admin_users row is bound to this subject yet,
	// so it must fall back to the email match and bind oidc_subject.
	token, err := store.LoginWithOIDC(ctx, subject, email)
	if err != nil {
		t.Fatalf("first oidc login: %v", err)
	}
	gotID, err := store.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if gotID != adminID {
		t.Fatalf("expected admin %s, got %s", adminID, gotID)
	}

	var boundSubject string
	if err := pool.QueryRow(ctx, `SELECT oidc_subject FROM admin_users WHERE id = $1`, adminID).Scan(&boundSubject); err != nil {
		t.Fatalf("query bound subject: %v", err)
	}
	if boundSubject != subject {
		t.Fatalf("expected oidc_subject %q bound, got %q", subject, boundSubject)
	}

	// Second OIDC login: now resolved via the fast oidc_subject path,
	// which doesn't care what email is passed (proves the subject lookup
	// is actually being used, not a repeat of the email fallback).
	token2, err := store.LoginWithOIDC(ctx, subject, "irrelevant@sirfi.test")
	if err != nil {
		t.Fatalf("second oidc login: %v", err)
	}
	gotID2, err := store.Authenticate(ctx, token2)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if gotID2 != adminID {
		t.Fatalf("expected admin %s, got %s", adminID, gotID2)
	}
}

func TestLoginWithOIDCDisabledAdminRejected(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	store := adminauth.New(pool, 12*time.Hour)

	email := fmt.Sprintf("ops-%s@sirfi.test", uuid.New())
	adminID, err := store.CreateAdmin(ctx, email, "some-password-never-used")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE admin_users SET disabled_at = now() WHERE id = $1`, adminID); err != nil {
		t.Fatalf("disable admin: %v", err)
	}

	if _, err := store.LoginWithOIDC(ctx, "sub-"+uuid.New().String(), email); err != adminauth.ErrInvalidCredentials {
		t.Fatalf("expected ErrInvalidCredentials for a disabled admin, got %v", err)
	}
}
