package config_test

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/sirfi/payment-engine-v2/internal/platform/config"
)

// validTestKey is 32 bytes of hex — a well-formed value for
// TENANT_SECRET_ENCRYPTION_KEY, distinct from the local-dev default so it
// doesn't trip the production placeholder check. Computed from a byte
// slice rather than a hand-counted literal so its length can't drift.
var validTestKey = func() string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	return hex.EncodeToString(b)
}()

type mapSource map[string]string

func (m mapSource) Get(key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

func TestLoad_MissingRequired(t *testing.T) {
	_, err := config.Load(mapSource{})
	if err == nil {
		t.Fatal("expected an error when required fields are missing")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("expected DATABASE_URL error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "TENANT_SECRET_ENCRYPTION_KEY is required") {
		t.Fatalf("expected TENANT_SECRET_ENCRYPTION_KEY error, got: %v", err)
	}
}

func TestLoad_DefaultsApplied(t *testing.T) {
	cfg, err := config.Load(mapSource{
		"DATABASE_URL":                 "postgres://real:real@realhost:5432/real",
		"TENANT_SECRET_ENCRYPTION_KEY": validTestKey,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Environment != "development" {
		t.Fatalf("expected default environment 'development', got %q", cfg.Environment)
	}
	if cfg.HMACClockSkew != 5*time.Minute {
		t.Fatalf("expected default HMACClockSkew 5m, got %v", cfg.HMACClockSkew)
	}
	if cfg.AdminSessionTTL != 12*time.Hour {
		t.Fatalf("expected default AdminSessionTTL 12h, got %v", cfg.AdminSessionTTL)
	}
	if cfg.EventbusBatchSize != 50 {
		t.Fatalf("expected default EventbusBatchSize 50, got %d", cfg.EventbusBatchSize)
	}
	if len(cfg.TenantSecretEncryptionKey) != 32 {
		t.Fatalf("expected a 32-byte decoded key, got %d bytes", len(cfg.TenantSecretEncryptionKey))
	}
}

func TestLoad_OverridesApplied(t *testing.T) {
	cfg, err := config.Load(mapSource{
		"DATABASE_URL":                 "postgres://real:real@realhost:5432/real",
		"TENANT_SECRET_ENCRYPTION_KEY": validTestKey,
		"HMAC_CLOCK_SKEW":              "10m",
		"ADMIN_SESSION_TTL":            "1h",
		"EVENTBUS_BATCH_SIZE":          "200",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.HMACClockSkew != 10*time.Minute {
		t.Fatalf("expected overridden HMACClockSkew 10m, got %v", cfg.HMACClockSkew)
	}
	if cfg.AdminSessionTTL != time.Hour {
		t.Fatalf("expected overridden AdminSessionTTL 1h, got %v", cfg.AdminSessionTTL)
	}
	if cfg.EventbusBatchSize != 200 {
		t.Fatalf("expected overridden EventbusBatchSize 200, got %d", cfg.EventbusBatchSize)
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	_, err := config.Load(mapSource{
		"DATABASE_URL":                 "postgres://real:real@realhost:5432/real",
		"TENANT_SECRET_ENCRYPTION_KEY": validTestKey,
		"HMAC_CLOCK_SKEW":              "not-a-duration",
	})
	if err == nil {
		t.Fatal("expected an error for invalid duration")
	}
	if !strings.Contains(err.Error(), "HMAC_CLOCK_SKEW") {
		t.Fatalf("expected HMAC_CLOCK_SKEW error, got: %v", err)
	}
}

func TestLoad_RejectsDevDatabaseURLInProduction(t *testing.T) {
	_, err := config.Load(mapSource{
		"APP_ENV":                      "production",
		"DATABASE_URL":                 "postgres://payment_engine:local_dev_only@localhost:5433/2settle?sslmode=disable",
		"TENANT_SECRET_ENCRYPTION_KEY": validTestKey,
	})
	if err == nil {
		t.Fatal("expected an error when the local-dev DATABASE_URL is used in production")
	}
	if !strings.Contains(err.Error(), "local-dev default") {
		t.Fatalf("expected local-dev-default error, got: %v", err)
	}
}

func TestLoad_DevDatabaseURLAllowedOutsideProduction(t *testing.T) {
	_, err := config.Load(mapSource{
		"DATABASE_URL":                 "postgres://payment_engine:local_dev_only@localhost:5433/2settle?sslmode=disable",
		"TENANT_SECRET_ENCRYPTION_KEY": validTestKey,
	})
	if err != nil {
		t.Fatalf("expected local-dev DATABASE_URL to be fine outside production, got: %v", err)
	}
}

func TestLoad_RejectsDevEncryptionKeyInProduction(t *testing.T) {
	_, err := config.Load(mapSource{
		"APP_ENV":                      "production",
		"DATABASE_URL":                 "postgres://real:real@realhost:5432/real",
		"TENANT_SECRET_ENCRYPTION_KEY": "REPLACE_WITH_YOUR_OWN_32_BYTE_HEX_KEY",
	})
	if err == nil {
		t.Fatal("expected an error when the local-dev encryption key is used in production")
	}
	if !strings.Contains(err.Error(), "local-dev default") {
		t.Fatalf("expected local-dev-default error, got: %v", err)
	}
}

func TestLoad_RejectsWrongLengthEncryptionKey(t *testing.T) {
	_, err := config.Load(mapSource{
		"DATABASE_URL":                 "postgres://real:real@realhost:5432/real",
		"TENANT_SECRET_ENCRYPTION_KEY": "deadbeef",
	})
	if err == nil {
		t.Fatal("expected an error for a too-short encryption key")
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("expected a 32-byte-length error, got: %v", err)
	}
}

func TestLoad_RejectsNonHexEncryptionKey(t *testing.T) {
	_, err := config.Load(mapSource{
		"DATABASE_URL":                 "postgres://real:real@realhost:5432/real",
		"TENANT_SECRET_ENCRYPTION_KEY": "not-hex-at-all!!",
	})
	if err == nil {
		t.Fatal("expected an error for a non-hex encryption key")
	}
	if !strings.Contains(err.Error(), "invalid hex") {
		t.Fatalf("expected an invalid-hex error, got: %v", err)
	}
}

func TestLoad_CollectsMultipleErrors(t *testing.T) {
	_, err := config.Load(mapSource{
		"HMAC_CLOCK_SKEW":     "garbage",
		"EVENTBUS_BATCH_SIZE": "also-garbage",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"DATABASE_URL", "TENANT_SECRET_ENCRYPTION_KEY", "HMAC_CLOCK_SKEW", "EVENTBUS_BATCH_SIZE"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected combined error to mention %s, got: %v", want, msg)
		}
	}
}
