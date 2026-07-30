package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/session"
)

var (
	// ErrNotRetryable means RetryPayout was called against a settlement
	// that isn't currently eligible: not settlement_failed, and not a
	// still-settling row an ops analyst has explicitly confirmed genuinely
	// failed (confirmedFailed=true — the bucket-3 path, ARCHITECTURE.md §8).
	ErrNotRetryable = errors.New("settlement: not eligible for retry")
	// ErrReversalNotFound / ErrReversalAlreadyResolved gate ResolveReversal.
	ErrReversalNotFound        = errors.New("settlement: reversal case not found")
	ErrReversalAlreadyResolved = errors.New("settlement: reversal case already resolved")
)

// RetryPayout is the ops-triggered retry path (ARCHITECTURE.md §8): either
// settlement_failed with the auto-retry cap exhausted, or a still-settling
// row whose dispatched attempt has timed out (bucket 3) and a human has
// confirmed with the provider that the original attempt genuinely failed —
// §8 is explicit that bucket 3 is "never auto-retried" and only proceeds
// "after a human confirms". confirmedFailed=true is how that confirmation
// is expressed; it's required, not inferred, when the row is still
// 'settling'. attempt_count is left as-is (not reset) — the idempotency key
// already encodes the attempt number, so the next dispatch simply becomes
// attempt N+1.
func (s *Store) RetryPayout(ctx context.Context, settlementID, adminID uuid.UUID, correctedDestination json.RawMessage, confirmedFailed bool) (*Settlement, error) {
	st, err := s.GetSettlement(ctx, settlementID)
	if err != nil {
		return nil, err
	}

	var from Status
	switch {
	case st.Status == StatusSettlementFailed:
		from = StatusSettlementFailed
	case st.Status == StatusSettling && st.OpsPagedAt != nil && confirmedFailed:
		from = StatusSettling
	default:
		return nil, ErrNotRetryable
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE settlements SET status = 'pending_dispatch', ops_paged_at = NULL,
		   pending_destination = $3, updated_at = now()
		 WHERE id = $1 AND status = $2`,
		settlementID, string(from), correctedDestination,
	)
	if err != nil {
		return nil, fmt.Errorf("settlement: retry payout: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotRetryable
	}

	// settlement.Status and session.Status are distinct named types sharing
	// the same underlying type (string) and the same vocabulary for these
	// in-flight/terminal names (ARCHITECTURE.md §8) — a direct conversion,
	// not a lookup.
	if _, err := s.sessionStore.RetryToSettling(ctx, st.SessionID, session.Status(from)); err != nil {
		return nil, fmt.Errorf("settlement: retry payout: transition session: %w", err)
	}

	return s.GetSettlement(ctx, settlementID)
}

// AccountRef identifies a ledger account for ResolveReversal's manual
// posting — an explicit account reference rather than an inferred one,
// since ARCHITECTURE.md §8 only says a reversal resolution lands "wherever
// the funds actually ended up" without specifying a fixed taxonomy.
type AccountRef struct {
	TenantID    *uuid.UUID
	AccountType string
	AssetCode   string
	UnitType    string // "fiat" or "crypto"
	Name        string
}

// ResolveReversal closes an open settlement_reversals case
// (ARCHITECTURE.md §8's reversal_resolved): posts a manual_adjustment
// ledger entry wherever ops says the funds actually ended up, marks the
// reversal case resolved, and moves the session to its terminal
// reversal_resolved state. Does not release the deposit reservation — it
// was already released at the original settled (rule 5: reversed doesn't
// re-release the address).
func (s *Store) ResolveReversal(ctx context.Context, reversalID, adminID uuid.UUID, debit, credit AccountRef, amount decimal.Decimal, note string) error {
	var settlementID, sessionID uuid.UUID
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT settlement_id, session_id, status FROM settlement_reversals WHERE id = $1`,
		reversalID,
	).Scan(&settlementID, &sessionID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrReversalNotFound
	}
	if err != nil {
		return fmt.Errorf("settlement: resolve reversal: %w", err)
	}
	if status != "open" {
		return ErrReversalAlreadyResolved
	}

	debitAccountID, err := s.ledger.GetOrCreateAccount(ctx, debit.TenantID, debit.AccountType, debit.AssetCode, debit.UnitType, debit.Name)
	if err != nil {
		return fmt.Errorf("settlement: resolve reversal: resolve debit account: %w", err)
	}
	creditAccountID, err := s.ledger.GetOrCreateAccount(ctx, credit.TenantID, credit.AccountType, credit.AssetCode, credit.UnitType, credit.Name)
	if err != nil {
		return fmt.Errorf("settlement: resolve reversal: resolve credit account: %w", err)
	}

	if _, err := s.ledger.Post(ctx, ledger.Transaction{
		IdempotencyKey: fmt.Sprintf("manual_adjustment:reversal_%s", reversalID),
		TxnType:        "manual_adjustment",
		ReferenceType:  "settlement_reversal",
		ReferenceID:    reversalID,
		CreatedBy:      "settlement",
		Entries: []ledger.Entry{
			{AccountID: debitAccountID, Direction: ledger.Debit, Amount: amount, AssetCode: debit.AssetCode},
			{AccountID: creditAccountID, Direction: ledger.Credit, Amount: amount, AssetCode: credit.AssetCode},
		},
	}); err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return fmt.Errorf("settlement: resolve reversal: post manual_adjustment: %w", err)
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE settlement_reversals SET status = 'resolved', resolved_by = $2, resolved_at = now(), resolution_note = $3
		 WHERE id = $1 AND status = 'open'`,
		reversalID, adminID, note,
	)
	if err != nil {
		return fmt.Errorf("settlement: resolve reversal: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrReversalAlreadyResolved
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE settlements SET status = 'reversal_resolved', updated_at = now() WHERE id = $1 AND status = 'reversed'`,
		settlementID,
	); err != nil {
		return fmt.Errorf("settlement: resolve reversal: update settlement: %w", err)
	}

	if _, err := s.sessionStore.TransitionToReversalResolved(ctx, sessionID); err != nil {
		return fmt.Errorf("settlement: resolve reversal: transition session: %w", err)
	}
	return nil
}
