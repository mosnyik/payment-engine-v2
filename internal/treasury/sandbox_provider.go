package treasury

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const sandboxProviderName = "sandbox"

// sandboxConfirmDelay/sandboxConfirmPollInterval are compiled-in, not ops
// knobs — the sandbox environment (docs/SANDBOX_PLAN.md) is a fixed-behavior
// test surface, same reasoning session.SessionTTL and settlement's retry-
// policy constants already establish for design decisions that shouldn't
// vary per deployment.
const (
	sandboxConfirmDelay        = 10 * time.Second
	sandboxConfirmPollInterval = 3 * time.Second
)

// sandboxCollectionProvider is the fake CollectionProvider registered only
// when Config.SandboxMode is set (see New) — hands back a fake-looking
// address instead of calling a real chain/partner API. Deposits against it
// are never actually detected on-chain; SandboxConfirmJob below confirms
// them on a timer instead.
type sandboxCollectionProvider struct{}

func (sandboxCollectionProvider) Name() string             { return sandboxProviderName }
func (sandboxCollectionProvider) IsEnabled() bool          { return true }
func (sandboxCollectionProvider) CustodyType() CustodyType { return CustodyTypePartner }

func (sandboxCollectionProvider) ReserveAddress(_ context.Context, _ uuid.UUID, cryptoAsset, cryptoNetwork string) (ProviderAddress, error) {
	ref := uuid.New().String()
	return ProviderAddress{
		Address:           fmt.Sprintf("sandbox-%s-%s-%s", cryptoNetwork, cryptoAsset, ref[:8]),
		ProviderReference: ref,
	}, nil
}

// SandboxConfirmJob is the timer-based fake-deposit confirmer: sandboxConfirmDelay
// after a reservation is made against the sandbox collection provider, it
// synthesizes a confirmed deposit sized to exactly cover the owning
// session's locked fiat amount, driving the rest of the pipeline
// (deposit_confirmed -> settlement) with no real chain activity or webhook
// call involved. Shaped like session.TTLJob/settlement.DispatchWorker —
// started with `go job.Run(ctx)` from main.go, only when SandboxMode is set.
type SandboxConfirmJob struct {
	store *Store
}

func NewSandboxConfirmJob(store *Store) *SandboxConfirmJob {
	return &SandboxConfirmJob{store: store}
}

func (j *SandboxConfirmJob) Run(ctx context.Context) {
	ticker := time.NewTicker(sandboxConfirmPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.store.confirmDueSandboxDeposits(ctx); err != nil {
				log.Printf("treasury: sandbox confirm sweep: %v", err)
			}
		}
	}
}

// confirmDueSandboxDeposits finds every sandbox reservation old enough to
// "arrive" with no deposit recorded yet, computes the crypto amount that
// exactly matches the owning session's locked fiat amount (joining straight
// into sessions/rate_locks by SQL rather than importing the session/rate
// packages just for this one read — same directly-against-the-table
// convention compliance.travelRuleThresholdExceeded already uses), and
// confirms it through the same state-machine entrypoint the real Busha
// webhook uses (recordDepositTransition, webhook.go).
func (s *Store) confirmDueSandboxDeposits(ctx context.Context) error {
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, s.fiat_amount, rl.rate, rl.asset_price_usd
		 FROM treasury_address_reservations r
		 JOIN sessions s ON s.deposit_reservation_id = r.id
		 JOIN rate_locks rl ON rl.id = s.rate_lock_id
		 WHERE r.provider_name = $1 AND r.status = 'reserved' AND r.reserved_at < $2
		   AND NOT EXISTS (SELECT 1 FROM treasury_deposits d WHERE d.reservation_id = r.id)`,
		sandboxProviderName, time.Now().Add(-sandboxConfirmDelay),
	)
	if err != nil {
		return fmt.Errorf("treasury: find due sandbox deposits: %w", err)
	}
	type due struct {
		reservationID uuid.UUID
		fiatAmount    decimal.Decimal
		rate          decimal.Decimal
		assetPriceUSD decimal.Decimal
	}
	var dues []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.reservationID, &d.fiatAmount, &d.rate, &d.assetPriceUSD); err != nil {
			rows.Close()
			return fmt.Errorf("treasury: scan due sandbox deposit: %w", err)
		}
		dues = append(dues, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("treasury: find due sandbox deposits: %w", err)
	}

	payload, err := json.Marshal(map[string]bool{"sandbox": true})
	if err != nil {
		return fmt.Errorf("treasury: marshal sandbox deposit payload: %w", err)
	}
	for _, d := range dues {
		// Mirrors rate.Lock.FiatToCrypto's formula: fiat -> USD -> crypto.
		cryptoAmount := d.fiatAmount.Div(d.rate).Div(d.assetPriceUSD)
		txReference := "sandbox-" + d.reservationID.String()
		if err := s.recordDepositTransition(ctx, d.reservationID, "confirmed", cryptoAmount, txReference, payload); err != nil {
			return fmt.Errorf("treasury: confirm sandbox deposit %s: %w", d.reservationID, err)
		}
	}
	return nil
}
