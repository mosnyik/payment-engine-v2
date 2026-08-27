package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sirfi/payment-engine-v2/internal/compliance"
	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/settlement"
	"github.com/sirfi/payment-engine-v2/internal/treasury"
)

// sandboxCorridorSpec is one parsed entry from config.Config.SandboxCorridors.
type sandboxCorridorSpec struct {
	cryptoAsset   string
	cryptoNetwork string
	fiatCurrency  string
}

// parseSandboxCorridors parses a comma-separated list of
// "cryptoAsset:cryptoNetwork:fiatCurrency" triples (config.Config.SandboxCorridors's
// format, e.g. "USDT:SANDBOX:NGN,BTC:SANDBOX:NGN").
func parseSandboxCorridors(spec string) ([]sandboxCorridorSpec, error) {
	var specs []sandboxCorridorSpec
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("sandbox: invalid SANDBOX_CORRIDORS entry %q — want cryptoAsset:cryptoNetwork:fiatCurrency", entry)
		}
		specs = append(specs, sandboxCorridorSpec{
			cryptoAsset:   strings.TrimSpace(parts[0]),
			cryptoNetwork: strings.TrimSpace(parts[1]),
			fiatCurrency:  strings.TrimSpace(parts[2]),
		})
	}
	return specs, nil
}

// seedSandboxCorridors upserts every corridor named in spec, with its
// compliance/collection/settlement provider bindings all pointed at the
// fake sandbox providers (see each package's sandbox_provider.go) — the
// config-driven mechanism corridor.UpsertCorridor/UpsertProviderBinding
// were already built for (see their doc comments), just never wired to a
// config source until now. Idempotent — safe to run on every boot, same as
// db.Migrate above it. Only called when cfg.SandboxMode is set.
//
// Also seeds a permissive (empty required_fields) jurisdiction_kyb_requirements
// row per sandbox corridor's fiat currency (Phase 10) — sandbox KYB
// submissions still must declare a non-empty declared_currencies, just with
// nothing actually required in it, so sandbox onboarding isn't blocked by
// requirements the sandbox doesn't know about.
func seedSandboxCorridors(ctx context.Context, corridorStore *corridor.Store, complianceStore *compliance.Store, spec string) error {
	specs, err := parseSandboxCorridors(spec)
	if err != nil {
		return err
	}
	for _, s := range specs {
		if _, err := complianceStore.UpsertJurisdictionRequirement(ctx, s.fiatCurrency, "sandbox", []string{}); err != nil {
			return fmt.Errorf("sandbox: upsert jurisdiction requirement for %s: %w", s.fiatCurrency, err)
		}
		corridorID, err := corridorStore.UpsertCorridor(ctx, corridor.UpsertCorridorInput{
			CryptoAsset:   s.cryptoAsset,
			CryptoNetwork: s.cryptoNetwork,
			FiatCurrency:  s.fiatCurrency,
			Active:        true,
			// No Travel Rule threshold — a sandbox corridor has nothing to
			// enforce it against. ComplianceHoldTimeout still needs a sane
			// value even though the sandbox compliance provider auto-approves
			// every session and should never actually hold one.
			TravelRuleWindow:      time.Hour,
			ComplianceHoldTimeout: 24 * time.Hour,
		})
		if err != nil {
			return fmt.Errorf("sandbox: upsert corridor %s/%s/%s: %w", s.cryptoAsset, s.cryptoNetwork, s.fiatCurrency, err)
		}

		bindings := []struct {
			providerType corridor.ProviderType
			providerName string
		}{
			{corridor.ProviderTypeCompliance, compliance.SandboxProviderName},
			{corridor.ProviderTypeCollection, treasury.SandboxProviderName},
			{corridor.ProviderTypeSettlement, settlement.SandboxProviderName},
		}
		for _, b := range bindings {
			if _, err := corridorStore.UpsertProviderBinding(ctx, corridorID, b.providerType, b.providerName, 0, true, nil); err != nil {
				return fmt.Errorf("sandbox: upsert %s provider binding for %s/%s/%s: %w", b.providerType, s.cryptoAsset, s.cryptoNetwork, s.fiatCurrency, err)
			}
		}
	}
	return nil
}
