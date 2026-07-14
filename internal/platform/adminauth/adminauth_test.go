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

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set — skipping integration test")
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
