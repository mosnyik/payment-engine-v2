// Package config is the single place secrets and tunable settings are
// loaded and validated at startup. Every other package receives values from
// a *Config passed in by main.go — nothing reaches for os.Getenv itself.
// Swapping the source (env vars today, a real secrets manager later) means
// implementing Source once; no call site anywhere else changes.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// localDevDatabaseURL matches .env.example — if this is still the value in
// production, someone deployed without setting a real DATABASE_URL.
const localDevDatabaseURL = "postgres://payment_engine:local_dev_only@localhost:5433/2settle?sslmode=disable"

// localDevTenantSecretEncryptionKey matches .env.example — same
// dev-default-in-production rejection as localDevDatabaseURL.
const localDevTenantSecretEncryptionKey = "REPLACE_WITH_YOUR_OWN_32_BYTE_HEX_KEY"

type Config struct {
	// Environment is "development" (default) or "production", read from
	// APP_ENV. Gates the weak/placeholder-secret checks below.
	Environment string

	DatabaseURL string

	// TenantSecretEncryptionKey encrypts tenant credentials (e.g. HMAC
	// secrets) at rest — see internal/platform/crypto. Interim measure, not
	// a KMS: still a key colocated with app config, same limitation flagged
	// for the HD wallet seed key in ARCHITECTURE.md §2, just applied here
	// too rather than leaving tenant secrets in plaintext.
	TenantSecretEncryptionKey []byte

	// Operational tuning — previously hardcoded constants in their owning
	// packages, now settable per-environment without a rebuild.
	HMACClockSkew     time.Duration // default 5m
	AdminSessionTTL   time.Duration // default 12h
	EventbusBatchSize int           // default 50

	HTTPAddr string // default :3700

	RateEngine RateEngineConfig
	Treasury   TreasuryConfig
}

// RateProviderConfig configures one external HTTP rate-provider adapter
// (Busha, LiquidRamp, Anchor) — see internal/rate. All default disabled:
// none of these have a real endpoint/response shape wired in yet.
type RateProviderConfig struct {
	Enabled bool
	APIURL  string
	APIKey  string
}

// RateEngineConfig is the rate engine's operational tuning — see
// ARCHITECTURE.md §7.
type RateEngineConfig struct {
	CoinMarketCapAPIKey string
	FetchInterval       time.Duration // default 30s
	Busha               RateProviderConfig
	LiquidRamp          RateProviderConfig
	Anchor              RateProviderConfig
}

// CollectionProviderConfig configures one external crypto-collection
// adapter (see internal/treasury) — a different credential surface from
// RateProviderConfig even for the same partner (Busha), since rate-quote
// and collection are separate APIs. Default disabled: no real endpoint or
// webhook signature scheme is wired in yet.
type CollectionProviderConfig struct {
	Enabled       bool
	APIURL        string
	APIKey        string
	WebhookSecret string
}

// TreasuryConfig is treasury's operational tuning — see internal/treasury
// and ARCHITECTURE.md §2/§3.
type TreasuryConfig struct {
	Busha CollectionProviderConfig

	// HDWalletSeedEncryptionKey decrypts the singleton hd_wallet_seed row
	// (see internal/treasury.LoadHDWalletSeed) — same interim
	// key-in-config pattern as TenantSecretEncryptionKey, structurally
	// separated from the ciphertext it protects (which lives in the DB,
	// not here), fixing the exact v1 audit gap where both sat in the same
	// .env file.
	HDWalletSeedEncryptionKey []byte

	Bitcoin  ChainConfig
	Ethereum ChainConfig
	BSC      ChainConfig
	Tron     ChainConfig

	Watcher WatcherConfig
	Sweep   SweepConfig
}

// ChainConfig configures one self-custody chain watcher/broadcaster. APIURL
// is a REST explorer endpoint for bitcoin/ethereum/bsc (Blockstream /
// Etherscan V2 / an EVM JSON-RPC URL) or a gRPC host:port for tron
// (TronGrid). All default disabled — self-custody stays off until both a
// seed is loaded and the specific chain is turned on.
type ChainConfig struct {
	Enabled          bool
	APIURL           string
	APIKey           string
	MinConfirmations int
}

// WatcherConfig is the self-custody deposit watcher's operational tuning.
type WatcherConfig struct {
	PollInterval time.Duration // default 30s, matches v1's cadence
}

// SweepConfig is the stable-asset batch-sweep policy's tuning (volatile
// assets always sweep immediately — see internal/treasury/sweep.go). This
// volatile/stable split has no v1 equivalent (confirmed absent there); the
// defaults below are this system's own choice, not ported.
type SweepConfig struct {
	// StableBalanceThreshold is a decimal string (parsed by
	// internal/treasury, not here, to keep this package free of the
	// decimal dependency) — once a stable-asset address's confirmed,
	// unswept balance reaches this, it's eligible for a batch sweep.
	StableBalanceThreshold string
	// StableTimeBackstop sweeps a stable-asset address regardless of
	// balance once its oldest unswept confirmed deposit is this old.
	StableTimeBackstop time.Duration
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

	tenantSecretEncryptionKeyHex := requireString(source, "TENANT_SECRET_ENCRYPTION_KEY", &errs)
	if environment == "production" && tenantSecretEncryptionKeyHex == localDevTenantSecretEncryptionKey {
		errs = append(errs, "TENANT_SECRET_ENCRYPTION_KEY is still the local-dev default from .env.example — set a real value for production")
	}
	var tenantSecretEncryptionKey []byte
	if tenantSecretEncryptionKeyHex != "" {
		key, err := hex.DecodeString(tenantSecretEncryptionKeyHex)
		if err != nil {
			errs = append(errs, fmt.Sprintf("TENANT_SECRET_ENCRYPTION_KEY: invalid hex: %v", err))
		} else if len(key) != 32 {
			errs = append(errs, fmt.Sprintf("TENANT_SECRET_ENCRYPTION_KEY: must decode to 32 bytes, got %d", len(key)))
		} else {
			tenantSecretEncryptionKey = key
		}
	}

	hmacClockSkew := durationOrDefault(source, "HMAC_CLOCK_SKEW", 5*time.Minute, &errs)
	adminSessionTTL := durationOrDefault(source, "ADMIN_SESSION_TTL", 12*time.Hour, &errs)
	eventbusBatchSize := intOrDefault(source, "EVENTBUS_BATCH_SIZE", 50, &errs)
	httpAddr := stringOrDefault(source, "HTTP_ADDR", ":3700")

	rateEngine := RateEngineConfig{
		CoinMarketCapAPIKey: stringOrDefault(source, "COINMARKETCAP_API_KEY", ""),
		FetchInterval:       durationOrDefault(source, "RATE_FETCH_INTERVAL", 30*time.Second, &errs),
		Busha:               loadRateProviderConfig(source, "BUSHA"),
		LiquidRamp:          loadRateProviderConfig(source, "LIQUIDRAMP"),
		Anchor:              loadRateProviderConfig(source, "ANCHOR"),
	}

	// Optional, unlike TENANT_SECRET_ENCRYPTION_KEY — self-custody has no
	// HTTP routes wired yet (see internal/treasury package doc), so a
	// deployment that never sets this simply can't call LoadHDWalletSeed,
	// not a startup failure. Validated the same way when present, though:
	// a malformed key here is exactly as dangerous as a malformed tenant
	// key, just not yet load-bearing for every deployment.
	hdWalletSeedEncryptionKeyHex := stringOrDefault(source, "HD_WALLET_SEED_ENCRYPTION_KEY", "")
	var hdWalletSeedEncryptionKey []byte
	if hdWalletSeedEncryptionKeyHex != "" {
		key, err := hex.DecodeString(hdWalletSeedEncryptionKeyHex)
		if err != nil {
			errs = append(errs, fmt.Sprintf("HD_WALLET_SEED_ENCRYPTION_KEY: invalid hex: %v", err))
		} else if len(key) != 32 {
			errs = append(errs, fmt.Sprintf("HD_WALLET_SEED_ENCRYPTION_KEY: must decode to 32 bytes, got %d", len(key)))
		} else {
			hdWalletSeedEncryptionKey = key
		}
	}

	treasury := TreasuryConfig{
		Busha:                     loadCollectionProviderConfig(source, "BUSHA_TREASURY"),
		HDWalletSeedEncryptionKey: hdWalletSeedEncryptionKey,
		Bitcoin:                   loadChainConfig(source, "BITCOIN", 2),
		Ethereum:                  loadChainConfig(source, "ETHEREUM", 12),
		BSC:                       loadChainConfig(source, "BSC", 15),
		Tron:                      loadChainConfig(source, "TRON", 19),
		Watcher: WatcherConfig{
			PollInterval: durationOrDefault(source, "WATCHER_POLL_INTERVAL", 30*time.Second, &errs),
		},
		Sweep: SweepConfig{
			StableBalanceThreshold: stringOrDefault(source, "SWEEP_STABLE_BALANCE_THRESHOLD", "1000"),
			StableTimeBackstop:     durationOrDefault(source, "SWEEP_STABLE_TIME_BACKSTOP", 6*time.Hour, &errs),
		},
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}

	return &Config{
		Environment:               environment,
		DatabaseURL:               databaseURL,
		TenantSecretEncryptionKey: tenantSecretEncryptionKey,
		HMACClockSkew:             hmacClockSkew,
		AdminSessionTTL:           adminSessionTTL,
		EventbusBatchSize:         eventbusBatchSize,
		HTTPAddr:                  httpAddr,
		RateEngine:                rateEngine,
		Treasury:                  treasury,
	}, nil
}

// loadChainConfig reads an optional, disabled-by-default self-custody
// chain's settings — minConfirmationsDefault matches v1's hardcoded
// per-chain thresholds (BTC 2, ETH 12, BSC 15, TRX 19), now tunable
// per-environment the same way HMAC_CLOCK_SKEW etc. already are.
func loadChainConfig(source Source, prefix string, minConfirmationsDefault int) ChainConfig {
	var errs []string // discarded: a malformed value here safely falls back to the default, same reasoning loadRateProviderConfig uses
	return ChainConfig{
		Enabled:          boolOrDefault(source, prefix+"_ENABLED", false),
		APIURL:           stringOrDefault(source, prefix+"_API_URL", ""),
		APIKey:           stringOrDefault(source, prefix+"_API_KEY", ""),
		MinConfirmations: intOrDefault(source, prefix+"_MIN_CONFIRMATIONS", minConfirmationsDefault, &errs),
	}
}

// loadRateProviderConfig reads an optional, disabled-by-default external
// rate provider's settings. Unlike the required config above, a malformed
// *_RATE_ENABLED value defaulting to "disabled" is safe — these providers
// have no real endpoint wired in yet — so it's not collected as an error.
func loadRateProviderConfig(source Source, prefix string) RateProviderConfig {
	return RateProviderConfig{
		Enabled: boolOrDefault(source, prefix+"_RATE_ENABLED", false),
		APIURL:  stringOrDefault(source, prefix+"_RATE_API_URL", ""),
		APIKey:  stringOrDefault(source, prefix+"_RATE_API_KEY", ""),
	}
}

// loadCollectionProviderConfig reads an optional, disabled-by-default
// external collection provider's settings — same malformed-value-defaults-
// safely reasoning as loadRateProviderConfig, since none of these have a
// real endpoint/webhook scheme wired in yet.
func loadCollectionProviderConfig(source Source, prefix string) CollectionProviderConfig {
	return CollectionProviderConfig{
		Enabled:       boolOrDefault(source, prefix+"_ENABLED", false),
		APIURL:        stringOrDefault(source, prefix+"_API_URL", ""),
		APIKey:        stringOrDefault(source, prefix+"_API_KEY", ""),
		WebhookSecret: stringOrDefault(source, prefix+"_WEBHOOK_SECRET", ""),
	}
}

func boolOrDefault(source Source, key string, def bool) bool {
	v, ok := source.Get(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
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
