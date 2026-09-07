package treasury

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

var (
	ErrInvalidWebhookSignature = errors.New("treasury: invalid webhook signature")
	ErrUnknownReservation      = errors.New("treasury: webhook references an unknown reservation")
	ErrInvalidDepositEvent     = errors.New("treasury: invalid Busha deposit event")
)

// DepositEventType identifies a Busha Business transfer event.
type DepositEventType string

const (
	DepositEventDetected  DepositEventType = "transfer.processing"
	DepositEventConfirmed DepositEventType = "transfer.funds_received"
)

// bushaWebhookPayload is the Busha Business event envelope.
type bushaWebhookPayload struct {
	EventType DepositEventType `json:"event"`
	Data      bushaTransfer    `json:"data"`
}

// ComputeWebhookSignature retains the hexadecimal format for tenant notifications.
func ComputeWebhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ComputeBushaSignature implements Busha Business's base64 HMAC-SHA256.
// Tenant notifications retain their existing hexadecimal signature format.
func ComputeBushaSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func VerifyBushaSignature(secret string, body []byte, signature string) error {
	if strings.TrimSpace(secret) == "" || signature == "" || !hmac.Equal([]byte(ComputeBushaSignature(secret, body)), []byte(signature)) {
		return ErrInvalidWebhookSignature
	}
	return nil
}

// VerifyWebhookSignature checks signatureHeader against body using a
// constant-time compare — every provider callback must be
// signature-verified, no exceptions (ARCHITECTURE.md §4, audit lesson #4).
func VerifyWebhookSignature(secret string, body []byte, signatureHeader string) error {
	if strings.TrimSpace(secret) == "" || signatureHeader == "" {
		return ErrInvalidWebhookSignature
	}
	expected := ComputeWebhookSignature(secret, body)
	if !hmac.Equal([]byte(expected), []byte(signatureHeader)) {
		return ErrInvalidWebhookSignature
	}
	return nil
}

// HandleDepositWebhook processes one Busha deposit callback: verifies the
// signature, resolves it to a reservation, then applies a
// WHERE status = <expected> compare-and-set transition (never a blind
// write — audit lesson #1). Idempotent on (reservation_id, tx_reference)
// so a replayed webhook is a no-op rather than double-processing the same
// deposit; a 'confirmed' arriving before its 'detected' still lands
// correctly since the row is created directly as 'confirmed' in that case.
func (s *Store) HandleDepositWebhook(ctx context.Context, body []byte, signatureHeader string) error {
	provider, ok := s.providers["busha"]
	if !ok || !provider.IsEnabled() {
		return ErrInvalidWebhookSignature
	}
	if err := VerifyBushaSignature(s.bushaWebhookSecret, body, signatureHeader); err != nil {
		return err
	}

	var payload bushaWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("%w: malformed JSON", ErrInvalidDepositEvent)
	}
	status, relevant, err := classifyBushaDeposit(payload)
	if err != nil {
		return err
	}
	if !relevant {
		return nil
	}
	transfer := payload.Data
	reservationID, err := s.findReservationByProviderReference(ctx, transfer.ID)
	if err != nil {
		return err
	}
	r, err := s.GetReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	if transfer.SourceCurrency != r.CryptoAsset || transfer.TargetCurrency != r.CryptoAsset ||
		(transfer.PayIn.Network != "" && transfer.PayIn.Network != r.CryptoNetwork) ||
		(transfer.PayIn.Address != "" && transfer.PayIn.Address != r.Address) ||
		(transfer.PayIn.Type != "" && transfer.PayIn.Type != "address") {
		return fmt.Errorf("%w: transfer does not match reservation", ErrInvalidDepositEvent)
	}
	// Transfer IDs are stable across processing, funds_received and completed;
	// blockchain hashes are not present in every event. Never count one transfer twice.
	return s.recordDepositTransition(ctx, reservationID, status, transfer.SourceAmount, transfer.ID, body)
}

func classifyBushaDeposit(payload bushaWebhookPayload) (string, bool, error) {
	status := ""
	want := ""
	switch payload.EventType {
	case DepositEventDetected:
		status, want = "detected", "processing"
	case DepositEventConfirmed:
		status, want = "confirmed", "funds_received"
	case "transfer.completed":
		status, want = "confirmed", "completed"
	default:
		return "", false, nil // pending is not evidence of a deposit
	}
	t := payload.Data
	if t.ID == "" || t.Status != want || !t.SourceAmount.IsPositive() || !t.SourceAmount.Equal(t.SourceAmount.Round(8)) || t.SourceCurrency == "" || t.TargetCurrency == "" {
		return "", false, ErrInvalidDepositEvent
	}
	return status, true, nil
}

// recordDepositTransition is the shared state-machine step both the Busha
// webhook handler above and the self-custody watcher (watcher.go) use:
// idempotent insert (or compare-and-set confirm) for one deposit, keyed on
// (reservation_id, tx_reference). status must be "detected" or "confirmed".
// A "confirmed" call with no prior "detected" row lands directly as
// confirmed (see HandleDepositWebhook's doc comment) — one state machine,
// two writers.
//
// Runs in one transaction so a real state transition and the
// treasury.deposit_detected/confirmed event describing it (published only
// when this call actually changed something, never on a redelivered
// duplicate) are atomic — Phase 5's session module is the consumer.
func (s *Store) recordDepositTransition(ctx context.Context, reservationID uuid.UUID, status string, amount decimal.Decimal, txReference string, rawPayload []byte) error {
	var detectedAt, confirmedAt *time.Time
	now := time.Now()
	if status == "confirmed" {
		confirmedAt = &now
	} else {
		detectedAt = &now
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("treasury: begin deposit transition: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	inserted, err := s.insertDepositIfAbsent(ctx, tx, reservationID, status, amount, txReference, detectedAt, confirmedAt, rawPayload)
	if err != nil {
		return err
	}

	transitioned := inserted
	if !inserted && status == "confirmed" {
		// The row already existed (created by an earlier 'detected' write) and
		// this call confirms it — compare-and-set, never a blind write.
		tag, err := tx.Exec(ctx,
			`UPDATE treasury_deposits SET status = 'confirmed', confirmed_at = $3, amount = $4, provider_payload = $5, updated_at = now()
			 WHERE reservation_id = $1 AND tx_reference = $2 AND status = 'detected'`,
			reservationID, txReference, now, amount, rawPayload,
		)
		if err != nil {
			return fmt.Errorf("treasury: confirm deposit: %w", err)
		}
		transitioned = tag.RowsAffected() > 0
	}

	if transitioned && s.bus != nil {
		eventType := "treasury.deposit_detected"
		if status == "confirmed" {
			eventType = "treasury.deposit_confirmed"
		}
		payload, err := json.Marshal(map[string]string{
			"tx_reference": txReference,
			"amount":       amount.String(),
		})
		if err != nil {
			return fmt.Errorf("treasury: marshal deposit event payload: %w", err)
		}
		if err := s.bus.Publish(ctx, tx, eventbus.Event{
			EventType:     eventType,
			AggregateType: "treasury_deposit",
			AggregateID:   reservationID,
			Payload:       payload,
		}); err != nil {
			return fmt.Errorf("treasury: publish deposit event: %w", err)
		}
	}

	return tx.Commit(ctx)
}

func (s *Store) findReservationByProviderReference(ctx context.Context, providerReference string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM treasury_address_reservations
		 WHERE provider_name = 'busha' AND provider_reference = $1 AND provider_reference <> ''
		 ORDER BY reserved_at DESC LIMIT 1`,
		providerReference,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrUnknownReservation
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("treasury: find reservation: %w", err)
	}
	return id, nil
}

func (s *Store) insertDepositIfAbsent(ctx context.Context, tx pgx.Tx, reservationID uuid.UUID, status string, amount decimal.Decimal, txReference string, detectedAt, confirmedAt *time.Time, rawPayload []byte) (bool, error) {
	tag, err := tx.Exec(ctx,
		`INSERT INTO treasury_deposits
		   (reservation_id, status, crypto_asset, amount, tx_reference, provider_payload, detected_at, confirmed_at)
		 SELECT $1, $2, r.crypto_asset, $3, $4, $5, $6, $7
		 FROM treasury_address_reservations r WHERE r.id = $1
		 ON CONFLICT (reservation_id, tx_reference) DO NOTHING`,
		reservationID, status, amount, txReference, rawPayload, detectedAt, confirmedAt,
	)
	if err != nil {
		return false, fmt.Errorf("treasury: insert deposit: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// GetDeposit looks up a deposit by reservation + tx reference — used by
// tests and, later, ops tooling.
func (s *Store) GetDeposit(ctx context.Context, reservationID uuid.UUID, txReference string) (*Deposit, error) {
	d := &Deposit{ReservationID: reservationID, TxReference: txReference}
	err := s.pool.QueryRow(ctx,
		`SELECT id, status, crypto_asset, amount, provider_payload, detected_at, confirmed_at
		 FROM treasury_deposits WHERE reservation_id = $1 AND tx_reference = $2`,
		reservationID, txReference,
	).Scan(&d.ID, &d.Status, &d.CryptoAsset, &d.Amount, &d.ProviderPayload, &d.DetectedAt, &d.ConfirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("treasury: no deposit found for reservation %s / tx %s", reservationID, txReference)
	}
	if err != nil {
		return nil, fmt.Errorf("treasury: get deposit: %w", err)
	}
	return d, nil
}
