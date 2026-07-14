package corridor_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

func openTestPool(t *testing.T) *db.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")

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

// uniqueNetwork keeps each test's corridor natural key from colliding with
// data left over from a previous run against the same live database.
func uniqueNetwork(t *testing.T) string {
	t.Helper()
	return "TESTNET_" + t.Name()
}

func TestUpsertAndGetCorridor(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := corridor.New(pool)

	network := uniqueNetwork(t)

	id, err := s.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:           "USDT",
		CryptoNetwork:         network,
		FiatCurrency:          "NGN",
		Active:                true,
		TravelRuleWindow:      24 * time.Hour,
		ComplianceHoldTimeout: 48 * time.Hour,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}

	got, err := s.GetCorridor(ctx, "USDT", network, "NGN")
	if err != nil {
		t.Fatalf("get corridor: %v", err)
	}
	if got.ID != id {
		t.Fatalf("expected id %s, got %s", id, got.ID)
	}
	if got.ComplianceHoldTimeout != 48*time.Hour {
		t.Fatalf("expected 48h hold timeout, got %v", got.ComplianceHoldTimeout)
	}
	if got.TravelRuleWindow != 24*time.Hour {
		t.Fatalf("expected 24h travel rule window, got %v", got.TravelRuleWindow)
	}

	// Same natural key, different settings — must update in place, not
	// create a second row (the whole point of upsert-by-natural-key).
	_, err = s.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:           "USDT",
		CryptoNetwork:         network,
		FiatCurrency:          "NGN",
		Active:                true,
		ComplianceHoldTimeout: 12 * time.Hour,
	})
	if err != nil {
		t.Fatalf("re-upsert corridor: %v", err)
	}

	got2, err := s.GetCorridor(ctx, "USDT", network, "NGN")
	if err != nil {
		t.Fatalf("get corridor after re-upsert: %v", err)
	}
	if got2.ID != id {
		t.Fatalf("expected re-upsert to keep the same id %s, got %s", id, got2.ID)
	}
	if got2.ComplianceHoldTimeout != 12*time.Hour {
		t.Fatalf("expected updated hold timeout 12h, got %v", got2.ComplianceHoldTimeout)
	}
}

func TestGetCorridor_NotFound(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := corridor.New(pool)

	_, err := s.GetCorridor(ctx, "DOES", "NOT", "EXIST")
	if !errors.Is(err, corridor.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGetCorridor_InactiveNotReturned(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := corridor.New(pool)

	network := uniqueNetwork(t)
	_, err := s.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:   "BTC",
		CryptoNetwork: network,
		FiatCurrency:  "ZMW",
		Active:        false,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}

	_, err = s.GetCorridor(ctx, "BTC", network, "ZMW")
	if !errors.Is(err, corridor.ErrNotFound) {
		t.Fatalf("expected an inactive corridor to behave as not found, got %v", err)
	}
}

func TestProviderBindingsOrderedByPriorityAndFiltersInactive(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := corridor.New(pool)

	network := uniqueNetwork(t)
	corridorID, err := s.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:   "USDT",
		CryptoNetwork: network,
		FiatCurrency:  "NGN",
		Active:        true,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}

	mustBind := func(name string, priority int, active bool) {
		t.Helper()
		if _, err := s.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeSettlement, name, priority, active, nil); err != nil {
			t.Fatalf("upsert provider binding %s: %v", name, err)
		}
	}

	mustBind("providerC", 2, true)
	mustBind("providerA", 0, true)
	mustBind("providerB", 1, true)
	mustBind("providerDisabled", -1, false) // lower priority number but inactive — must be excluded

	bindings, err := s.ListActiveProviders(ctx, corridorID, corridor.ProviderTypeSettlement)
	if err != nil {
		t.Fatalf("list active providers: %v", err)
	}

	if len(bindings) != 3 {
		t.Fatalf("expected 3 active bindings, got %d: %+v", len(bindings), bindings)
	}
	wantOrder := []string{"providerA", "providerB", "providerC"}
	for i, want := range wantOrder {
		if bindings[i].ProviderName != want {
			t.Fatalf("expected binding %d to be %s, got %s", i, want, bindings[i].ProviderName)
		}
	}

	// A different provider_type on the same corridor must not see these bindings.
	rateBindings, err := s.ListActiveProviders(ctx, corridorID, corridor.ProviderTypeRate)
	if err != nil {
		t.Fatalf("list rate providers: %v", err)
	}
	if len(rateBindings) != 0 {
		t.Fatalf("expected no rate bindings, got %d", len(rateBindings))
	}
}

func TestUpsertProviderBinding_ConfigRoundTrips(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	s := corridor.New(pool)

	network := uniqueNetwork(t)
	corridorID, err := s.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
		CryptoAsset:   "USDT",
		CryptoNetwork: network,
		FiatCurrency:  "NGN",
		Active:        true,
	})
	if err != nil {
		t.Fatalf("upsert corridor: %v", err)
	}

	cfg, _ := json.Marshal(map[string]string{"api_base": "https://example.test"})
	if _, err := s.UpsertProviderBinding(ctx, corridorID, corridor.ProviderTypeCollection, "busha", 0, true, cfg); err != nil {
		t.Fatalf("upsert provider binding: %v", err)
	}

	bindings, err := s.ListActiveProviders(ctx, corridorID, corridor.ProviderTypeCollection)
	if err != nil {
		t.Fatalf("list active providers: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}

	var got map[string]string
	if err := json.Unmarshal(bindings[0].Config, &got); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if got["api_base"] != "https://example.test" {
		t.Fatalf("unexpected config: %+v", got)
	}
}
