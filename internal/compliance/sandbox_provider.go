package compliance

import "context"

// SandboxProviderName is the registered name of SandboxProvider — cmd/server
// forces submitKYB's provider_name to this when config.SandboxMode is set,
// so a sandbox tenant never needs to know a magic string to skip the manual
// hold queue.
const SandboxProviderName = "sandbox"

// SandboxProvider is the always-approve ScreeningProvider registered only
// when config.SandboxMode is set (see cmd/server/stores.go) — a Stripe-style
// sandbox environment (docs/SANDBOX_PLAN.md) has no real KYB vendor to call,
// so every case is approved outright rather than landing in the hold queue.
type SandboxProvider struct{}

func (SandboxProvider) Name() string { return SandboxProviderName }

func (SandboxProvider) Screen(ctx context.Context, c Case) (Decision, error) {
	return Decision{Approved: true, Reason: "sandbox: auto-approved"}, nil
}
