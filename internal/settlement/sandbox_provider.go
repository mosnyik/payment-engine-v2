package settlement

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

const sandboxProviderName = "sandbox"

// sandboxSettlementProvider is the fake SettlementProvider registered only
// when Config.SandboxMode is set (see New) — Dispatch always accepts
// immediately, since there is no real payout API to call in the sandbox
// environment (docs/SANDBOX_PLAN.md). Nothing ever calls back with a
// webhook, so confirmSandboxDispatches below closes the settling -> settled
// loop directly instead.
type sandboxSettlementProvider struct{}

func (sandboxSettlementProvider) Name() string    { return sandboxProviderName }
func (sandboxSettlementProvider) IsEnabled() bool { return true }

func (sandboxSettlementProvider) Dispatch(_ context.Context, _ PayoutRequest) (PayoutResult, error) {
	return PayoutResult{Outcome: OutcomeAccepted, ProviderReference: "sandbox-" + uuid.New().String()}, nil
}

// webhookSecret is never used: HandleSettlementWebhook only fires for real
// provider callbacks, which the sandbox provider never receives.
func (sandboxSettlementProvider) webhookSecret() string { return "" }

// confirmSandboxDispatches finalizes every settlement_attempts row
// dispatched to the sandbox provider and still waiting on a "confirmation"
// — normally that arrives as a signed webhook (webhook.go), but nothing
// calls back in sandbox mode, so DispatchWorker (dispatch.go) calls this on
// every tick instead, reusing the same settling -> settled transition
// handlePayoutSucceeded already implements for a real payout.succeeded
// webhook.
func (s *Store) confirmSandboxDispatches(ctx context.Context) error {
	rows, err := s.pool.Query(ctx,
		`SELECT id, settlement_id FROM settlement_attempts
		 WHERE provider_name = $1 AND status = 'dispatched'`,
		sandboxProviderName,
	)
	if err != nil {
		return fmt.Errorf("settlement: find due sandbox dispatches: %w", err)
	}
	var attempts []settlementAttempt
	for rows.Next() {
		a := settlementAttempt{ProviderName: sandboxProviderName}
		if err := rows.Scan(&a.ID, &a.SettlementID); err != nil {
			rows.Close()
			return fmt.Errorf("settlement: scan due sandbox dispatch: %w", err)
		}
		attempts = append(attempts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("settlement: find due sandbox dispatches: %w", err)
	}

	for _, a := range attempts {
		if err := s.handlePayoutSucceeded(ctx, &a); err != nil {
			return fmt.Errorf("settlement: confirm sandbox dispatch %s: %w", a.ID, err)
		}
	}
	return nil
}
