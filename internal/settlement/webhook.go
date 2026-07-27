package settlement

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

var (
	ErrInvalidWebhookSignature = errors.New("settlement: invalid webhook signature")
	ErrUnknownAttempt          = errors.New("settlement: webhook references an unknown settlement attempt")
	ErrUnknownProvider         = errors.New("settlement: unknown or unconfigured settlement provider")
)

// WebhookEventType is the event type a settlement provider's payout webhook
// reports. TODO: confirm these match each real provider's event names once
// known (see providers.go's per-provider TODO comments).
type WebhookEventType string

const (
	WebhookPayoutSucceeded WebhookEventType = "payout.succeeded"
	WebhookPayoutFailed    WebhookEventType = "payout.failed"
	WebhookPayoutReturned  WebhookEventType = "payout.returned"
)

// webhookPayload is a TODO-shaped inbound webhook body — replace with each
// provider's real payload once known, same swap providers.go's outbound
// request/response TODOs describe.
type webhookPayload struct {
	EventType         WebhookEventType `json:"event_type"`
	ProviderReference string           `json:"reference_id"`
	Amount            decimal.Decimal  `json:"amount"`
	FailureReason     string           `json:"failure_reason"`
}

// ComputeWebhookSignature computes the HMAC-SHA256 signature of body under
// secret. Mirrors treasury.ComputeWebhookSignature — same placeholder
// status until a real provider's signing scheme is known.
func ComputeWebhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature checks signatureHeader against body using a
// constant-time compare — every provider callback must be
// signature-verified, no exceptions (ARCHITECTURE.md §8 rule 4).
func VerifyWebhookSignature(secret string, body []byte, signatureHeader string) error {
	expected := ComputeWebhookSignature(secret, body)
	if !hmac.Equal([]byte(expected), []byte(signatureHeader)) {
		return ErrInvalidWebhookSignature
	}
	return nil
}

// HandleSettlementWebhook processes one payout-provider callback: verifies
// the signature using that provider's configured secret, resolves it to a
// settlement_attempts row, then applies the same WHERE status = <expected>
// compare-and-set discipline every transition in this codebase uses.
// Idempotent on redelivery — a webhook resolving an already-resolved
// attempt is a safe no-op, not an error.
func (s *Store) HandleSettlementWebhook(ctx context.Context, providerName string, body []byte, signatureHeader string) error {
	provider, ok := s.providers[providerName]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownProvider, providerName)
	}
	secret := provider.webhookSecret()
	if secret == "" {
		return fmt.Errorf("%w: %s has no webhook secret configured", ErrUnknownProvider, providerName)
	}
	if err := VerifyWebhookSignature(secret, body, signatureHeader); err != nil {
		return err
	}

	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("settlement: parse webhook payload: %w", err)
	}

	attempt, err := s.findAttemptByProviderReference(ctx, providerName, payload.ProviderReference)
	if err != nil {
		return err
	}

	switch payload.EventType {
	case WebhookPayoutSucceeded:
		return s.handlePayoutSucceeded(ctx, attempt)
	case WebhookPayoutFailed:
		return s.handleAttemptFailure(ctx, attempt, "failed_bucket2", payload.FailureReason, false)
	case WebhookPayoutReturned:
		return s.handlePayoutReturned(ctx, attempt, payload.FailureReason)
	default:
		return fmt.Errorf("settlement: unknown webhook event type %q", payload.EventType)
	}
}

type settlementAttempt struct {
	ID            uuid.UUID
	SettlementID  uuid.UUID
	AttemptNumber int
	ProviderName  string
	Status        string
}

func (s *Store) findAttemptByProviderReference(ctx context.Context, providerName, providerReference string) (*settlementAttempt, error) {
	a := &settlementAttempt{ProviderName: providerName}
	err := s.pool.QueryRow(ctx,
		`SELECT id, settlement_id, attempt_number, status FROM settlement_attempts
		 WHERE provider_name = $1 AND provider_reference = $2
		 ORDER BY dispatched_at DESC LIMIT 1`,
		providerName, providerReference,
	).Scan(&a.ID, &a.SettlementID, &a.AttemptNumber, &a.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnknownAttempt
	}
	if err != nil {
		return nil, fmt.Errorf("settlement: find attempt: %w", err)
	}
	return a, nil
}

// handlePayoutSucceeded: settling -> settled. Releases the deposit
// reservation (ARCHITECTURE.md §8 rule 5) and publishes settlement.completed.
func (s *Store) handlePayoutSucceeded(ctx context.Context, attempt *settlementAttempt) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE settlement_attempts SET status = 'succeeded', resolved_at = now()
		 WHERE id = $1 AND status = 'dispatched'`,
		attempt.ID,
	)
	if err != nil {
		return fmt.Errorf("settlement: mark attempt succeeded: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // redelivered/duplicate webhook — already resolved
	}

	st, err := s.GetSettlement(ctx, attempt.SettlementID)
	if err != nil {
		return err
	}

	transitioned, err := s.transitionSettlementAndPublish(ctx, st, StatusSettling, StatusSettled, "settlement.completed", nil)
	if err != nil {
		return err
	}
	if !transitioned {
		return nil
	}

	if _, err := s.sessionStore.TransitionToSettled(ctx, st.SessionID); err != nil {
		return fmt.Errorf("settlement: transition session to settled: %w", err)
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
	return nil
}

// handlePayoutReturned: settled -> reversed. A provider/bank return after an
// initial success (rare, §8) — reopens the tenant_payable liability and
// opens an ops case, but never unwinds the crypto side (rule 6).
func (s *Store) handlePayoutReturned(ctx context.Context, attempt *settlementAttempt, reason string) error {
	st, err := s.GetSettlement(ctx, attempt.SettlementID)
	if err != nil {
		return err
	}

	transitioned, err := s.transitionSettlementAndPublish(ctx, st, StatusSettled, StatusReversed, "settlement.reversed", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO settlement_reversals (settlement_id, session_id, reason) VALUES ($1, $2, $3)`,
			st.ID, st.SessionID, reason,
		)
		return err
	})
	if err != nil {
		return err
	}
	if !transitioned {
		return nil
	}

	if _, err := s.sessionStore.TransitionToReversed(ctx, st.SessionID); err != nil {
		return fmt.Errorf("settlement: transition session to reversed: %w", err)
	}

	accounts, err := s.resolveAccounts(ctx, st.TenantID, st.CryptoAsset, st.FiatCurrency)
	if err != nil {
		return err
	}
	_, err = s.ledger.Post(ctx, ledger.Transaction{
		IdempotencyKey: fmt.Sprintf("settlement_reversal:session_%s", st.SessionID),
		TxnType:        "reversal",
		ReferenceType:  "settlement",
		ReferenceID:    st.ID,
		CreatedBy:      "settlement",
		Entries: []ledger.Entry{
			{AccountID: accounts.TreasuryFiatOperating, Direction: ledger.Debit, Amount: st.TenantPayableAmount, AssetCode: st.FiatCurrency},
			{AccountID: accounts.TenantPayable, Direction: ledger.Credit, Amount: st.TenantPayableAmount, AssetCode: st.FiatCurrency},
		},
	})
	if err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return fmt.Errorf("settlement: post reversal: %w", err)
	}
	return nil
}

// transitionSettlementAndPublish is the shared CAS-transition-plus-publish
// shape for settlement.go's own status column — mirrors
// session.transitionToPending's begin/update/publish/commit pattern. extra,
// if non-nil, runs more writes (e.g. inserting a settlement_reversals row)
// inside the same transaction as the CAS update and the event publish.
func (s *Store) transitionSettlementAndPublish(ctx context.Context, st *Settlement, from, to Status, eventType string, extra func(pgx.Tx) error) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("settlement: begin transition to %s: %w", to, err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	tag, err := tx.Exec(ctx,
		`UPDATE settlements SET status = $3, updated_at = now() WHERE id = $1 AND status = $2`,
		st.ID, string(from), string(to),
	)
	if err != nil {
		return false, fmt.Errorf("settlement: transition to %s: %w", to, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	if extra != nil {
		if err := extra(tx); err != nil {
			return false, fmt.Errorf("settlement: transition to %s: extra: %w", to, err)
		}
	}

	if s.bus != nil {
		// tenant_id lets notification (Phase 7) resolve a delivery
		// destination straight from the payload, without depending on
		// internal/settlement.
		payload, err := json.Marshal(map[string]string{
			"session_id": st.SessionID.String(),
			"tenant_id":  st.TenantID.String(),
		})
		if err != nil {
			return false, fmt.Errorf("settlement: marshal %s payload: %w", eventType, err)
		}
		if err := s.bus.Publish(ctx, tx, eventbus.Event{
			EventType:     eventType,
			AggregateType: "settlement",
			AggregateID:   st.ID,
			Payload:       payload,
		}); err != nil {
			return false, fmt.Errorf("settlement: publish %s: %w", eventType, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("settlement: commit transition to %s: %w", to, err)
	}
	return true, nil
}
