package config_test

import (
	"github.com/sirfi/payment-engine-v2/internal/platform/config"
	"testing"
)

func TestLoad_BushaSandbox(t *testing.T) {
	base := mapSource{"DATABASE_URL": "postgres://test:test@localhost/test", "TENANT_SECRET_ENCRYPTION_KEY": validTestKey,
		"SANDBOX_MODE": "true", "SANDBOX_COLLECTION_PROVIDER": "busha", "SANDBOX_CORRIDORS": "USDT:TRX:NGN",
		"BUSHA_TREASURY_ENABLED": "true", "BUSHA_TREASURY_API_URL": "https://api.sandbox.busha.so",
		"BUSHA_TREASURY_API_KEY": "test-key", "BUSHA_TREASURY_WEBHOOK_SECRET": "test-secret"}
	if cfg, err := config.Load(base); err != nil || cfg.SandboxCollectionProvider != "busha" {
		t.Fatalf("valid config: %v", err)
	}
	for key, value := range map[string]string{"BUSHA_TREASURY_ENABLED": "false", "BUSHA_TREASURY_API_KEY": "", "BUSHA_TREASURY_WEBHOOK_SECRET": "", "BUSHA_TREASURY_API_URL": "https://api.busha.co", "SANDBOX_COLLECTION_PROVIDER": "typo"} {
		copy := mapSource{}
		for k, v := range base {
			copy[k] = v
		}
		copy[key] = value
		if _, err := config.Load(copy); err == nil {
			t.Errorf("accepted invalid %s", key)
		}
	}
}
