package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
	"github.com/sirfi/payment-engine-v2/internal/ledger"
)

// settlementAccounts is the six ledger accounts one settlement's postings
// touch (ARCHITECTURE.md §6's account taxonomy / worked example).
type settlementAccounts struct {
	TreasuryInTransit     uuid.UUID
	CryptoFXClearing      uuid.UUID
	FiatFXClearing        uuid.UUID
	TreasuryFiatOperating uuid.UUID
	FeeRevenue            uuid.UUID
	TenantPayable         uuid.UUID
}

func (s *Store) resolveAccounts(ctx context.Context, tenantID uuid.UUID, cryptoAsset, fiatCurrency string) (*settlementAccounts, error) {
	var a settlementAccounts
	var err error
	if a.TreasuryInTransit, err = s.ledger.GetOrCreateAccount(ctx, nil, "treasury_in_transit", cryptoAsset, "crypto", "Treasury in-transit: "+cryptoAsset); err != nil {
		return nil, fmt.Errorf("settlement: resolve accounts: %w", err)
	}
	if a.CryptoFXClearing, err = s.ledger.GetOrCreateAccount(ctx, nil, "crypto_fx_clearing", cryptoAsset, "crypto", "Crypto FX clearing: "+cryptoAsset); err != nil {
		return nil, fmt.Errorf("settlement: resolve accounts: %w", err)
	}
	if a.FiatFXClearing, err = s.ledger.GetOrCreateAccount(ctx, nil, "fiat_fx_clearing", fiatCurrency, "fiat", "Fiat FX clearing: "+fiatCurrency); err != nil {
		return nil, fmt.Errorf("settlement: resolve accounts: %w", err)
	}
	if a.TreasuryFiatOperating, err = s.ledger.GetOrCreateAccount(ctx, nil, "treasury_fiat_operating", fiatCurrency, "fiat", "Treasury fiat operating: "+fiatCurrency); err != nil {
		return nil, fmt.Errorf("settlement: resolve accounts: %w", err)
	}
	if a.FeeRevenue, err = s.ledger.GetOrCreateAccount(ctx, nil, "fee_revenue", fiatCurrency, "fiat", "Fee revenue: "+fiatCurrency); err != nil {
		return nil, fmt.Errorf("settlement: resolve accounts: %w", err)
	}
	tid := tenantID
	if a.TenantPayable, err = s.ledger.GetOrCreateAccount(ctx, &tid, "tenant_payable", fiatCurrency, "fiat", "Tenant payable: "+fiatCurrency); err != nil {
		return nil, fmt.Errorf("settlement: resolve accounts: %w", err)
	}
	return &a, nil
}

// DispatchWorker is the ticker-driven ledger-claim-then-dispatch pipeline —
// see settlement.go's package doc for why this must be a background worker
// rather than an eventbus.Handler (ledger.Post always opens its own
// transaction, so it can never run inside a handler's supplied tx).
type DispatchWorker struct {
	store        *Store
	pollInterval time.Duration
}

func NewDispatchWorker(store *Store, pollInterval time.Duration) *DispatchWorker {
	return &DispatchWorker{store: store, pollInterval: pollInterval}
}

// Run blocks until ctx is cancelled, same shape as session.TTLJob/rate.FetchJob.
func (w *DispatchWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	// Checked once, not per-tick: whether the sandbox provider exists at all
	// is fixed at Store construction (settlement.New), so re-checking the
	// map every tick would just waste a lookup.
	_, sandboxActive := w.store.providers[SandboxProviderName]
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.dispatchBatch(ctx)
			if sandboxActive {
				if err := w.store.confirmSandboxDispatches(ctx); err != nil {
					log.Printf("settlement: confirm sandbox dispatches: %v", err)
				}
			}
		}
	}
}

// dispatchBatch drains every currently pending_dispatch settlement in one
// tick — a single small table, so a CAS claim is sufficient without
// eventbus's FOR UPDATE SKIP LOCKED batching (that becomes the fallback if
// this worker is ever run from more than one server instance concurrently).
func (w *DispatchWorker) dispatchBatch(ctx context.Context) {
	for {
		claimed, err := w.store.claimNextPendingDispatch(ctx)
		if err != nil {
			log.Printf("settlement: dispatch batch: %v", err)
			return
		}
		if claimed == nil {
			return
		}
		if err := w.store.processSettlement(ctx, claimed); err != nil {
			log.Printf("settlement: process settlement %s: %v", claimed.ID, err)
		}
	}
}

func (s *Store) claimNextPendingDispatch(ctx context.Context) (*Settlement, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE settlements SET status = 'dispatching', updated_at = now()
		 WHERE id = (SELECT id FROM settlements WHERE status = 'pending_dispatch' ORDER BY created_at LIMIT 1)
		 RETURNING `+settlementColumns,
	)
	st, err := scanSettlement(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("settlement: claim next pending dispatch: %w", err)
	}
	return st, nil
}

// processSettlement is steps 1-8 of the plan: load the session's confirmed
// deposit and rate lock, compute the fee split, post the previously-deferred
// deposit_confirmed/fx_conversion ledger transactions (ARCHITECTURE.md §6's
// worked example rows 1-2), transition both settlement and session to
// settling, then dispatch the first payout attempt.
func (s *Store) processSettlement(ctx context.Context, st *Settlement) error {
	sess, err := s.sessionStore.GetSession(ctx, st.SessionID)
	if err != nil {
		return fmt.Errorf("settlement: get session: %w", err)
	}
	if sess.DepositReservationID == nil || sess.RateLockID == nil {
		return fmt.Errorf("settlement: session %s missing deposit reservation or rate lock", sess.ID)
	}

	cryptoAmount, err := s.treasuryStore.GetConfirmedDepositTotal(ctx, *sess.DepositReservationID)
	if err != nil {
		return fmt.Errorf("settlement: get confirmed deposit total: %w", err)
	}

	lock, err := s.rateStore.GetLock(ctx, *sess.RateLockID)
	if err != nil {
		return fmt.Errorf("settlement: get rate lock: %w", err)
	}
	fiatValue := lock.CryptoToFiat(cryptoAmount)

	feeBps, err := s.feeResolver.EffectiveFeeBps(ctx, sess.TenantID, sess.CorridorID)
	if err != nil {
		return fmt.Errorf("settlement: get effective fee bps: %w", err)
	}
	// tenantPayable is derived by subtraction, never independently rounded
	// — ledger.Post rejects any transaction whose entries don't net to
	// exactly zero per asset, so feeAmount + tenantPayable must equal
	// fiatValue exactly.
	feeAmount := fiatValue.Mul(decimal.NewFromInt(int64(feeBps))).Div(decimal.NewFromInt(10000)).Round(2)
	tenantPayable := fiatValue.Sub(feeAmount)

	accounts, err := s.resolveAccounts(ctx, sess.TenantID, sess.CryptoAsset, sess.FiatCurrency)
	if err != nil {
		return err
	}

	if _, err := s.ledger.Post(ctx, ledger.Transaction{
		IdempotencyKey: fmt.Sprintf("deposit_confirmed:session_%s", sess.ID),
		TxnType:        "deposit_confirmed",
		ReferenceType:  "session",
		ReferenceID:    sess.ID,
		CreatedBy:      "settlement",
		Entries: []ledger.Entry{
			{AccountID: accounts.TreasuryInTransit, Direction: ledger.Debit, Amount: cryptoAmount, AssetCode: sess.CryptoAsset},
			{AccountID: accounts.CryptoFXClearing, Direction: ledger.Credit, Amount: cryptoAmount, AssetCode: sess.CryptoAsset},
		},
	}); err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return fmt.Errorf("settlement: post deposit_confirmed: %w", err)
	}

	if _, err := s.ledger.Post(ctx, ledger.Transaction{
		IdempotencyKey: fmt.Sprintf("fx_conversion:session_%s", sess.ID),
		TxnType:        "fx_conversion",
		ReferenceType:  "session",
		ReferenceID:    sess.ID,
		CreatedBy:      "settlement",
		Entries: []ledger.Entry{
			{AccountID: accounts.FiatFXClearing, Direction: ledger.Debit, Amount: fiatValue, AssetCode: sess.FiatCurrency},
			{AccountID: accounts.TenantPayable, Direction: ledger.Credit, Amount: tenantPayable, AssetCode: sess.FiatCurrency},
			{AccountID: accounts.FeeRevenue, Direction: ledger.Credit, Amount: feeAmount, AssetCode: sess.FiatCurrency},
		},
	}); err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return fmt.Errorf("settlement: post fx_conversion: %w", err)
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE settlements SET status = 'settling', crypto_asset = $2, crypto_amount = $3,
		   fiat_currency = $4, fiat_value = $5, fee_amount = $6, tenant_payable_amount = $7, updated_at = now()
		 WHERE id = $1 AND status = 'dispatching'`,
		st.ID, sess.CryptoAsset, cryptoAmount, sess.FiatCurrency, fiatValue, feeAmount, tenantPayable,
	); err != nil {
		return fmt.Errorf("settlement: update to settling: %w", err)
	}
	if _, err := s.sessionStore.TransitionToSettling(ctx, sess.ID); err != nil {
		return fmt.Errorf("settlement: transition session to settling: %w", err)
	}

	st.TenantID, st.CorridorID = sess.TenantID, sess.CorridorID
	st.CryptoAsset, st.CryptoAmount = sess.CryptoAsset, cryptoAmount
	st.FiatCurrency, st.FiatValue = sess.FiatCurrency, fiatValue
	st.FeeAmount, st.TenantPayableAmount = feeAmount, tenantPayable
	st.Status = StatusSettling

	return s.dispatchAttempt(ctx, st)
}

// selectProvider prefers an untried, enabled provider (failover — §8:
// "failing over to the next provider is preferred over re-trying the one
// that just failed"). If every enabled provider has already been tried at
// least once (e.g. only one is configured), it falls back to the
// highest-priority enabled one — the caller only reaches this state when
// attempts remain under the cap, so retrying the same provider is correct
// there, not a bug. Returns nil, nil if no provider is enabled at all.
func (s *Store) selectProvider(ctx context.Context, corridorID uuid.UUID, tried map[string]bool) (SettlementProvider, error) {
	bindings, err := s.corridorStore.ListActiveProviders(ctx, corridorID, corridor.ProviderTypeSettlement)
	if err != nil {
		return nil, fmt.Errorf("settlement: list active providers: %w", err)
	}

	var enabled []SettlementProvider
	for _, b := range bindings {
		if p, ok := s.providers[b.ProviderName]; ok && p.IsEnabled() {
			enabled = append(enabled, p)
		}
	}
	if len(enabled) == 0 {
		return nil, nil
	}
	for _, p := range enabled {
		if !tried[p.Name()] {
			return p, nil
		}
	}
	return enabled[0], nil
}

func (s *Store) triedProviderNames(ctx context.Context, settlementID uuid.UUID) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT provider_name FROM settlement_attempts WHERE settlement_id = $1`,
		settlementID,
	)
	if err != nil {
		return nil, fmt.Errorf("settlement: tried providers: %w", err)
	}
	defer rows.Close()

	tried := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("settlement: tried providers: scan: %w", err)
		}
		tried[name] = true
	}
	return tried, rows.Err()
}

// dispatchAttempt is steps 9-14 of the plan: claim the ledger's
// pre-dispatch idempotency key (ARCHITECTURE.md §6's double-payout fix)
// before ever calling the provider, then dispatch and route the outcome.
func (s *Store) dispatchAttempt(ctx context.Context, st *Settlement) error {
	tried, err := s.triedProviderNames(ctx, st.ID)
	if err != nil {
		return err
	}
	provider, err := s.selectProvider(ctx, st.CorridorID, tried)
	if err != nil {
		return err
	}
	if provider == nil {
		return s.failSettlement(ctx, st, "no active, enabled settlement provider configured for this corridor")
	}

	accounts, err := s.resolveAccounts(ctx, st.TenantID, st.CryptoAsset, st.FiatCurrency)
	if err != nil {
		return err
	}

	attemptNumber := st.AttemptCount + 1
	idempotencyKey := fmt.Sprintf("settlement_payout:session_%s:attempt_%d", st.SessionID, attemptNumber)

	// The atomic pre-dispatch claim: must succeed before the provider is
	// ever called. ErrAlreadyPosted means a prior crash-and-resume of this
	// worker already claimed this attempt — abort without dispatching again.
	if _, err := s.ledger.Post(ctx, ledger.Transaction{
		IdempotencyKey: idempotencyKey,
		TxnType:        "settlement_payout",
		ReferenceType:  "session",
		ReferenceID:    st.SessionID,
		CreatedBy:      "settlement",
		Entries: []ledger.Entry{
			{AccountID: accounts.TenantPayable, Direction: ledger.Debit, Amount: st.TenantPayableAmount, AssetCode: st.FiatCurrency},
			{AccountID: accounts.TreasuryFiatOperating, Direction: ledger.Credit, Amount: st.TenantPayableAmount, AssetCode: st.FiatCurrency},
		},
	}); errors.Is(err, ledger.ErrAlreadyPosted) {
		return nil
	} else if err != nil {
		return fmt.Errorf("settlement: claim settlement_payout: %w", err)
	}

	attemptPayload := st.PendingDestination
	if attemptPayload == nil {
		attemptPayload = json.RawMessage("{}")
	}
	var attemptID uuid.UUID
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO settlement_attempts (settlement_id, attempt_number, provider_name, idempotency_key, provider_payload)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		st.ID, attemptNumber, provider.Name(), idempotencyKey, attemptPayload,
	).Scan(&attemptID); err != nil {
		return fmt.Errorf("settlement: insert attempt: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE settlements SET attempt_count = $2, updated_at = now() WHERE id = $1`,
		st.ID, attemptNumber,
	); err != nil {
		return fmt.Errorf("settlement: bump attempt count: %w", err)
	}
	st.AttemptCount = attemptNumber

	result, err := provider.Dispatch(ctx, PayoutRequest{
		SessionID:             st.SessionID,
		AttemptIdempotencyKey: idempotencyKey,
		FiatCurrency:          st.FiatCurrency,
		Amount:                st.TenantPayableAmount,
		// Destination: whatever ops supplied via RetryPayout's
		// corrected-details path, if this is a retry — nil on a first
		// attempt (no tenant-level payout-destination schema exists yet).
		Destination: st.PendingDestination,
	})
	if err != nil {
		return fmt.Errorf("settlement: dispatch to %q: %w", provider.Name(), err)
	}
	if st.PendingDestination != nil {
		if _, err := s.pool.Exec(ctx, `UPDATE settlements SET pending_destination = NULL WHERE id = $1`, st.ID); err != nil {
			return fmt.Errorf("settlement: clear pending destination: %w", err)
		}
		st.PendingDestination = nil
	}

	attempt := &settlementAttempt{ID: attemptID, SettlementID: st.ID, AttemptNumber: attemptNumber, ProviderName: provider.Name(), Status: "dispatched"}

	switch result.Outcome {
	case OutcomeAccepted:
		deadline := time.Now().Add(ConfirmationTimeout)
		_, err := s.transitionSettlementAndPublish(ctx, st, StatusSettling, StatusSettling, "settlement.dispatched", func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE settlement_attempts SET provider_reference = $2 WHERE id = $1`, attemptID, result.ProviderReference); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE settlements SET confirmation_deadline_at = $2 WHERE id = $1`, st.ID, deadline)
			return err
		})
		return err
	case OutcomeRejectedTerminal:
		return s.handleAttemptFailure(ctx, attempt, "failed_bucket4", result.FailureReason, true)
	default: // OutcomeRejectedRetryable
		return s.handleAttemptFailure(ctx, attempt, "failed_bucket1", result.FailureReason, false)
	}
}

// handleAttemptFailure is the shared bucket-1/2/4 failure path — called
// from dispatchAttempt (buckets 1/4, discovered synchronously) and from
// webhook.go's handler for a webhook-reported bucket-2 failure. CAS-marks
// the attempt, posts the compensating reversal, then either retries
// (buckets 1/2, cap not exhausted) or calls failSettlement (bucket 4, or
// buckets 1/2 with the cap exhausted).
func (s *Store) handleAttemptFailure(ctx context.Context, attempt *settlementAttempt, bucket, failureReason string, terminal bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE settlement_attempts SET status = $2, failure_reason = $3, resolved_at = now()
		 WHERE id = $1 AND status = 'dispatched'`,
		attempt.ID, bucket, failureReason,
	)
	if err != nil {
		return fmt.Errorf("settlement: mark attempt %s: %w", bucket, err)
	}
	if tag.RowsAffected() == 0 {
		return nil // redelivered/duplicate webhook, or already resolved by the synchronous dispatch path
	}

	st, err := s.GetSettlement(ctx, attempt.SettlementID)
	if err != nil {
		return err
	}

	accounts, err := s.resolveAccounts(ctx, st.TenantID, st.CryptoAsset, st.FiatCurrency)
	if err != nil {
		return err
	}
	if _, err := s.ledger.Post(ctx, ledger.Transaction{
		IdempotencyKey: fmt.Sprintf("settlement_reversal:session_%s:attempt_%d", st.SessionID, attempt.AttemptNumber),
		TxnType:        "reversal",
		ReferenceType:  "session",
		ReferenceID:    st.SessionID,
		CreatedBy:      "settlement",
		Entries: []ledger.Entry{
			{AccountID: accounts.TreasuryFiatOperating, Direction: ledger.Debit, Amount: st.TenantPayableAmount, AssetCode: st.FiatCurrency},
			{AccountID: accounts.TenantPayable, Direction: ledger.Credit, Amount: st.TenantPayableAmount, AssetCode: st.FiatCurrency},
		},
	}); err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return fmt.Errorf("settlement: post compensating reversal: %w", err)
	}

	if terminal || attempt.AttemptNumber >= MaxAutoRetryAttempts {
		return s.failSettlement(ctx, st, failureReason)
	}

	tried, err := s.triedProviderNames(ctx, st.ID)
	if err != nil {
		return err
	}
	hasUntried := false
	for name := range s.providers {
		if !tried[name] && s.providers[name].IsEnabled() {
			hasUntried = true
			break
		}
	}
	if !hasUntried {
		// No failover provider available — retry the same one after a
		// short backoff rather than immediately, per §8.
		time.Sleep(RetryBackoff)
	}
	return s.dispatchAttempt(ctx, st)
}

// failSettlement is the terminal settling -> settlement_failed path
// (ARCHITECTURE.md §8: bucket 4 immediately, or buckets 1/2 once the
// 3-attempt cap is exhausted). Releases the deposit reservation and pages
// ops right away, independent of sla_breached_at.
func (s *Store) failSettlement(ctx context.Context, st *Settlement, reason string) error {
	transitioned, err := s.transitionSettlementAndPublish(ctx, st, StatusSettling, StatusSettlementFailed, "settlement.failed", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE settlements SET ops_paged_at = now() WHERE id = $1`, st.ID)
		return err
	})
	if err != nil {
		return err
	}
	if !transitioned {
		return nil
	}

	if _, err := s.sessionStore.TransitionToSettlementFailed(ctx, st.SessionID); err != nil {
		return fmt.Errorf("settlement: transition session to settlement_failed: %w", err)
	}

	sess, err := s.sessionStore.GetSession(ctx, st.SessionID)
	if err != nil {
		return fmt.Errorf("settlement: get session for release: %w", err)
	}
	if sess.DepositReservationID != nil {
		if err := s.treasuryStore.ReleaseReservation(ctx, *sess.DepositReservationID); err != nil {
			return fmt.Errorf("settlement: release reservation: %w", err)
		}
	}
	log.Printf("settlement: %s settlement_failed, ops paged: %s", st.ID, reason)
	return nil
}
