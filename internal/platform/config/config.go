// Package config is the single place secrets and tunable settings are
// loaded and validated at startup. Every other package receives values from
// a *Config passed in by main.go — nothing reaches for os.Getenv itself.
// Swapping the source (env vars today, a real secrets manager later) means
// implementing Source once; no call site anywhere else changes.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// localDevDatabaseURL matches .env.example — if this is still the value in
// production, someone deployed without setting a real DATABASE_URL.
const localDevDatabaseURL = "postgres://payment_engine:local_dev_only@localhost:5433/payment_engine?sslmode=disable"

type Config struct {
	// Environment is "development" (default) or "production", read from
	// APP_ENV. Gates the weak/placeholder-secret checks below.
	Environment string

	DatabaseURL string

	// Operational tuning — previously hardcoded constants in their owning
	// packages, now settable per-environment without a rebuild.
	HMACClockSkew     time.Duration // default 5m
	AdminSessionTTL   time.Duration // default 12h
	EventbusBatchSize int           // default 50
}

func (c *Config) IsProduction() bool {
	return c.Environment == "production"
}

// Source abstracts where config values come from. EnvSource (below) is the
// only implementation today; a Vault/AWS-Secrets-Manager-backed Source can
// be added later and passed to Load without touching Config or any of its
// consumers.
type Source interface {
	Get(key string) (value string, ok bool)
}

type EnvSource struct{}

func (EnvSource) Get(key string) (string, bool) {
	return os.LookupEnv(key)
}

// LoadEnv is the normal entry point: loads .env if present (optional — real
// deployments set env vars directly, not via a file) then builds Config
// from the process environment.
func LoadEnv() (*Config, error) {
	_ = godotenv.Load()
	return Load(EnvSource{})
}

// Load builds and validates Config from source. It collects every problem
// found rather than failing on the first one, so a misconfigured
// environment can be fixed in one pass instead of one error at a time.
func Load(source Source) (*Config, error) {
	var errs []string

	environment := stringOrDefault(source, "APP_ENV", "development")

	databaseURL := requireString(source, "DATABASE_URL", &errs)
	if environment == "production" && databaseURL == localDevDatabaseURL {
		errs = append(errs, "DATABASE_URL is still the local-dev default from .env.example — set a real value for production")
	}

	hmacClockSkew := durationOrDefault(source, "HMAC_CLOCK_SKEW", 5*time.Minute, &errs)
	adminSessionTTL := durationOrDefault(source, "ADMIN_SESSION_TTL", 12*time.Hour, &errs)
	eventbusBatchSize := intOrDefault(source, "EVENTBUS_BATCH_SIZE", 50, &errs)

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return &Config{
		Environment:       environment,
		DatabaseURL:       databaseURL,
		HMACClockSkew:     hmacClockSkew,
		AdminSessionTTL:   adminSessionTTL,
		EventbusBatchSize: eventbusBatchSize,
	}, nil
}

func stringOrDefault(source Source, key, def string) string {
	if v, ok := source.Get(key); ok && v != "" {
		return v
	}
	return def
}

func requireString(source Source, key string, errs *[]string) string {
	v, ok := source.Get(key)
	if !ok || v == "" {
		*errs = append(*errs, key+" is required")
		return ""
	}
	return v
}

func durationOrDefault(source Source, key string, def time.Duration, errs *[]string) time.Duration {
	v, ok := source.Get(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: invalid duration %q: %v", key, v, err))
		return def
	}
	return d
}

func intOrDefault(source Source, key string, def int, errs *[]string) int {
	v, ok := source.Get(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: invalid integer %q: %v", key, v, err))
		return def
	}
	return n
}
