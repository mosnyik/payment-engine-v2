package settlement

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

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

// genericWebhookSignatureHeader is where the TODO-stub adapters' callbacks
// carry their HMAC signature. Real adapters (hyparrow.go) read their own
// headers in their own parseWebhook.
const genericWebhookSignatureHeader = "X-Webhook-Signature"

// webhookEventKind is what a provider callback means for the attempt it
// references, independent of how that provider names or shapes it.
type webhookEventKind int

const (
	// webhookIgnored: an event that doesn't affect settlement (e.g. a
	// wallet-funded or test event). Acknowledged, nothing else.
	webhookIgnored webhookEventKind = iota
	// webhookSucceeded: the payout landed.
	webhookSucceeded
	// webhookFailed: bucket 2 — definitively failed, nothing in flight.
	webhookFailed
	// webhookReturned: a return after an initial success.
	webhookReturned
	// webhookReversed: the provider says the funds came back. Treated as
	// webhookFailed while the attempt is still dispatched, and as
	// webhookReturned if the attempt had already succeeded.
	webhookReversed
	// webhookDeclined: declined, but the funds haven't come back yet and
	// the decline can still resolve either way — still in flight, so a
	// human is paged but nothing is retried or paid manually.
	webhookDeclined
)

// webhookEvent is a verified, provider-neutral payout callback.
type webhookEvent struct {
	Kind              webhookEventKind
	ProviderReference string
	Reason            string
}

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

// parseGenericWebhook is the TODO-stub adapters' shared callback scheme:
// hex HMAC-SHA256 of the raw body in genericWebhookSignatureHeader, body
// shaped like webhookPayload.
func parseGenericWebhook(providerName, secret string, header http.Header, body []byte) (webhookEvent, error) {
	if secret == "" {
		return webhookEvent{}, fmt.Errorf("%w: %s has no webhook secret configured", ErrUnknownProvider, providerName)
	}
	if err := VerifyWebhookSignature(secret, body, header.Get(genericWebhookSignatureHeader)); err != nil {
		return webhookEvent{}, err
	}

	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return webhookEvent{}, fmt.Errorf("settlement: parse webhook payload: %w", err)
	}

	ev := webhookEvent{ProviderReference: payload.ProviderReference, Reason: payload.FailureReason}
	switch payload.EventType {
	case WebhookPayoutSucceeded:
		ev.Kind = webhookSucceeded
	case WebhookPayoutFailed:
		ev.Kind = webhookFailed
	case WebhookPayoutReturned:
		ev.Kind = webhookReturned
	default:
		return webhookEvent{}, fmt.Errorf("settlement: unknown webhook event type %q", payload.EventType)
	}
	return ev, nil
}

// HandleSettlementWebhook processes one payout-provider callback: has the
// provider verify and parse it, resolves it to a settlement_attempts row,
// then applies the same WHERE status = <expected> compare-and-set
// discipline every transition in this codebase uses. Idempotent on
// redelivery — a webhook resolving an already-resolved attempt is a safe
// no-op, not an error.
func (s *Store) HandleSettlementWebhook(ctx context.Context, providerName string, body []byte, header http.Header) error {
	provider, ok := s.providers[providerName]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownProvider, providerName)
	}
	ev, err := provider.parseWebhook(header, body)
	if err != nil {
		return err
	}
	if ev.Kind == webhookIgnored {
		return nil
	}

	attempt, err := s.findAttemptByProviderReference(ctx, providerName, ev.ProviderReference)
	if err != nil {
		return err
	}

	// A failure can trigger an inline re-dispatch that first sleeps
	// RetryBackoff (handleAttemptFailure). If the provider gives up on the
	// request meanwhile, a cancelled ctx would abort that re-dispatch after
	// the compensating reversal but before the next attempt — leaving the
	// settlement settling with nothing in flight. Finish regardless.
	ctx = context.WithoutCancel(ctx)

	switch ev.Kind {
	case webhookSucceeded:
		return s.handlePayoutSucceeded(ctx, attempt)
	case webhookFailed:
		return s.handleAttemptFailure(ctx, attempt, "failed_bucket2", ev.Reason, false)
	case webhookReturned:
		return s.handlePayoutReturned(ctx, attempt, ev.Reason)
	case webhookReversed:
		if attempt.Status == "succeeded" {
			return s.handlePayoutReturned(ctx, attempt, ev.Reason)
		}
		return s.handleAttemptFailure(ctx, attempt, "failed_bucket2", ev.Reason, false)
	case webhookDeclined:
		return s.flagDeclinedAttempt(ctx, attempt, ev.ProviderReference, ev.Reason)
	default:
		return fmt.Errorf("settlement: unhandled webhook event kind %d", ev.Kind)
	}
}

// flagDeclinedAttempt records a provider decline that hasn't been reversed
// yet. The transfer can still land, so nothing is reversed, retried or
// handed to manual payout — ops are paged and told not to pay by hand.
// Deduped per attempt via failure_reason, so a redelivery doesn't re-alert.
func (s *Store) flagDeclinedAttempt(ctx context.Context, attempt *settlementAttempt, providerReference, reason string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE settlement_attempts SET failure_reason = $2
		 WHERE id = $1 AND status = 'dispatched' AND failure_reason IS NULL`,
		attempt.ID, reason,
	)
	if err != nil {
		return fmt.Errorf("settlement: flag declined attempt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE settlements SET ops_paged_at = COALESCE(ops_paged_at, now()) WHERE id = $1`,
		attempt.SettlementID,
	); err != nil {
		return fmt.Errorf("settlement: page ops for declined attempt: %w", err)
	}

	st, err := s.GetSettlement(ctx, attempt.SettlementID)
	if err != nil {
		return err
	}
	s.alertReview(ctx, st, providerReference, reason+". Awaiting reversal from the provider.")
	return nil
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
	return s.completeSessionSettlement(ctx, st)
}

// completeSessionSettlement is the session-side tail of any settled
// transition (provider success or manual payout): settling -> settled on the
// session, and release the deposit reservation (ARCHITECTURE.md §8 rule 5).
func (s *Store) completeSessionSettlement(ctx context.Context, st *Settlement) error {
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
	s.alertReversal(ctx, st, reason)

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
