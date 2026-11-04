package settlement

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sirfi/payment-engine-v2/internal/ledger"
)

// ManualProviderName is the provider_name recorded on the settlement_attempts
// row a manual (human-paid) settlement creates.
const ManualProviderName = "manual"

var (
	// ErrNotAwaitingManualPayout gates ManualSettle: only a settlement that
	// was handed to the Telegram fallback, and hasn't been retried or
	// settled since, can be marked paid by hand.
	ErrNotAwaitingManualPayout = errors.New("settlement: not awaiting manual payout")
	// ErrTelegramDisabled means no Telegram fallback (or no webhook secret
	// for its buttons) is configured.
	ErrTelegramDisabled = errors.New("settlement: telegram fallback not configured")
	// ErrInvalidTelegramSecret means the update didn't carry the
	// secret_token registered with Telegram's setWebhook.
	ErrInvalidTelegramSecret = errors.New("settlement: invalid telegram webhook secret")
)

// resolveDestination picks the bank details an attempt pays to: an
// ops-corrected destination from RetryPayout, else whatever the previous
// attempt used (so a correction sticks across later auto-retries), else the
// session's own payout_destination supplied at CreateSession.
func (s *Store) resolveDestination(ctx context.Context, st *Settlement) (json.RawMessage, error) {
	if len(st.PendingDestination) > 0 {
		return st.PendingDestination, nil
	}

	var last json.RawMessage
	err := s.pool.QueryRow(ctx,
		`SELECT provider_payload FROM settlement_attempts
		 WHERE settlement_id = $1 AND provider_payload <> '{}'::jsonb
		 ORDER BY attempt_number DESC LIMIT 1`,
		st.ID,
	).Scan(&last)
	if err == nil {
		return last, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("settlement: last attempt destination: %w", err)
	}

	sess, err := s.sessionStore.GetSession(ctx, st.SessionID)
	if err != nil {
		return nil, fmt.Errorf("settlement: get session destination: %w", err)
	}
	return sess.PayoutDestination, nil
}

// handToManualPayout is the Telegram fallback (ARCHITECTURE.md §8 "Manual
// payout fallback"): instead of settlement_failed, post the payout details
// to the ops group with a "Settlement completed" button and park the
// settlement in awaiting_manual_payout. The session stays settling — from
// the tenant's side the payout is still in progress. Only reached once
// nothing is in flight (every attempt rejected, reversed, or none possible).
//
// The alert is sent before the transition: if it can't be delivered,
// handled is false and the caller falls back to settlement_failed, same as
// v1 (a payout nobody was told about must not sit in a manual queue).
func (s *Store) handToManualPayout(ctx context.Context, st *Settlement, reason string) (handled bool, err error) {
	raw, err := s.resolveDestination(ctx, st)
	if err != nil {
		log.Printf("settlement: %s manual payout: %v", st.ID, err)
		return false, nil
	}
	dest, err := parseBankDestination(raw)
	if err != nil || dest.AccountNumber == "" {
		// Nothing a human could pay to — the ops queue (settlement_failed)
		// with RetryPayout's corrected-destination path is the right tool.
		log.Printf("settlement: %s manual payout: no usable bank details in destination", st.ID)
		return false, nil
	}

	text, keyboard := manualPayoutMessage(st, dest, reason)
	if err := s.telegram.sendMessage(ctx, text, keyboard); err != nil {
		log.Printf("settlement: %s manual payout alert: %v", st.ID, err)
		return false, nil
	}

	transitioned, err := s.transitionSettlementAndPublish(ctx, st, StatusSettling, StatusAwaitingManualPayout, "settlement.awaiting_manual_payout", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE settlements SET ops_paged_at = now() WHERE id = $1`, st.ID)
		return err
	})
	if err != nil {
		return false, err
	}
	if transitioned {
		log.Printf("settlement: %s awaiting manual payout via telegram: %s", st.ID, reason)
	}
	// Not transitioned: something else already moved it on. The alert's
	// button is harmless — ManualSettle's CAS refuses anything not
	// awaiting_manual_payout.
	return true, nil
}

// ManualSettle marks a settlement paid by hand: awaiting_manual_payout ->
// settled. Records a 'manual' attempt (settledBy goes in its payload as the
// audit trail), re-posts the settlement_payout ledger claim (the last
// automatic attempt's claim was reversed when it failed), then settles the
// session and releases the deposit reservation exactly like a provider
// success webhook. CAS-guarded, so a second press is refused, not doubled.
func (s *Store) ManualSettle(ctx context.Context, settlementID uuid.UUID, settledBy string) (*Settlement, error) {
	st, err := s.GetSettlement(ctx, settlementID)
	if err != nil {
		return nil, err
	}
	if st.Status != StatusAwaitingManualPayout {
		return st, fmt.Errorf("%w (status: %s)", ErrNotAwaitingManualPayout, st.Status)
	}

	idempotencyKey := fmt.Sprintf("settlement_payout:session_%s:manual", st.SessionID)
	auditPayload, err := json.Marshal(map[string]string{"settled_by": settledBy})
	if err != nil {
		return nil, err
	}
	attemptNumber := st.AttemptCount + 1

	transitioned, err := s.transitionSettlementAndPublish(ctx, st, StatusAwaitingManualPayout, StatusSettled, "settlement.completed", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO settlement_attempts (settlement_id, attempt_number, provider_name, idempotency_key, status, provider_payload, resolved_at)
			 VALUES ($1, $2, $3, $4, 'succeeded', $5, now())`,
			st.ID, attemptNumber, ManualProviderName, idempotencyKey, auditPayload,
		); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE settlements SET attempt_count = $2 WHERE id = $1`, st.ID, attemptNumber)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !transitioned {
		current, err := s.GetSettlement(ctx, settlementID)
		if err != nil {
			return nil, err
		}
		return current, fmt.Errorf("%w (status: %s)", ErrNotAwaitingManualPayout, current.Status)
	}

	accounts, err := s.resolveAccounts(ctx, st.TenantID, st.CryptoAsset, st.FiatCurrency)
	if err != nil {
		return nil, err
	}
	if _, err := s.ledger.Post(ctx, ledger.Transaction{
		IdempotencyKey: idempotencyKey,
		TxnType:        "settlement_payout",
		ReferenceType:  "session",
		ReferenceID:    st.SessionID,
		CreatedBy:      "settlement:manual",
		Entries: []ledger.Entry{
			{AccountID: accounts.TenantPayable, Direction: ledger.Debit, Amount: st.TenantPayableAmount, AssetCode: st.FiatCurrency},
			{AccountID: accounts.TreasuryFiatOperating, Direction: ledger.Credit, Amount: st.TenantPayableAmount, AssetCode: st.FiatCurrency},
		},
	}); err != nil && !errors.Is(err, ledger.ErrAlreadyPosted) {
		return nil, fmt.Errorf("settlement: post manual settlement_payout: %w", err)
	}

	if err := s.completeSessionSettlement(ctx, st); err != nil {
		return nil, err
	}
	log.Printf("settlement: %s settled manually by %s", st.ID, settledBy)
	return s.GetSettlement(ctx, settlementID)
}

// telegramUpdate is the slice of a Telegram Bot API Update this handles: a
// press of an inline button on one of the alerts.
type telegramUpdate struct {
	CallbackQuery *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		From struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"from"`
		Message *struct {
			MessageID int `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

// HandleTelegramUpdate processes one Telegram webhook delivery. Only button
// presses from the configured ops chat are acted on; everything else is
// acknowledged and ignored. Button outcomes (including refusals) are
// reported back to the presser via answerCallbackQuery rather than as an
// error, so Telegram doesn't redeliver them.
func (s *Store) HandleTelegramUpdate(ctx context.Context, secretToken string, body []byte) error {
	if s.telegram == nil || s.telegram.cfg.WebhookSecret == "" {
		return ErrTelegramDisabled
	}
	if subtle.ConstantTimeCompare([]byte(secretToken), []byte(s.telegram.cfg.WebhookSecret)) != 1 {
		return ErrInvalidTelegramSecret
	}

	var update telegramUpdate
	if err := json.Unmarshal(body, &update); err != nil {
		return fmt.Errorf("settlement: parse telegram update: %w", err)
	}
	cq := update.CallbackQuery
	if cq == nil || cq.ID == "" || cq.Data == "" {
		return nil
	}

	answer := func(text string, alert bool) {
		if err := s.telegram.answerCallback(ctx, cq.ID, text, alert); err != nil {
			log.Printf("settlement: telegram answer callback: %v", err)
		}
	}

	if cq.Message == nil || strconv.FormatInt(cq.Message.Chat.ID, 10) != s.telegram.cfg.ChatID {
		answer("This settlement action is not allowed here.", true)
		return nil
	}

	idText, ok := strings.CutPrefix(cq.Data, telegramSettleAction)
	if !ok {
		answer("Unknown settlement action.", true)
		return nil
	}
	settlementID, err := uuid.Parse(idText)
	if err != nil {
		answer("Unknown settlement.", true)
		return nil
	}

	by := "telegram:" + strconv.FormatInt(cq.From.ID, 10)
	if cq.From.Username != "" {
		by += " (@" + cq.From.Username + ")"
	}

	st, err := s.ManualSettle(ctx, settlementID, by)
	switch {
	case errors.Is(err, ErrNotFound):
		answer("Settlement not found.", true)
		return nil
	case errors.Is(err, ErrNotAwaitingManualPayout):
		answer(fmt.Sprintf("Not marked: this settlement is %s, not awaiting manual payout.", st.Status), true)
		return nil
	case err != nil:
		answer("Could not mark the settlement completed — try again.", true)
		return err
	}

	answer("Settlement marked as completed.", false)
	if err := s.telegram.clearButtons(ctx, cq.Message.Chat.ID, cq.Message.MessageID); err != nil {
		log.Printf("settlement: telegram clear buttons: %v", err)
	}
	if err := s.telegram.sendMessage(ctx, manualSettledMessage(st, by), nil); err != nil {
		log.Printf("settlement: telegram manual settled notice: %v", err)
	}
	return nil
}
