package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/ledger"
	"github.com/sirfi/payment-engine-v2/internal/session"
)

const (
	testTelegramChatID = "-1001234567890"
	testTelegramSecret = "tg-secret"
)

// fakeTelegram records every Bot API call. fail makes sendMessage error,
// simulating a Telegram outage.
type fakeTelegram struct {
	mu    sync.Mutex
	calls []telegramCall
	fail  bool
}

type telegramCall struct {
	Method string
	Body   map[string]any
}

func (f *fakeTelegram) handler(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.calls = append(f.calls, telegramCall{Method: method, Body: body})
	fail := f.fail
	f.mu.Unlock()
	if fail && method == "sendMessage" {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (f *fakeTelegram) byMethod(method string) []telegramCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []telegramCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (e *testEnv) withTelegram(t *testing.T) *fakeTelegram {
	t.Helper()
	f := &fakeTelegram{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	e.settlement.telegram = newTelegramNotifier(TelegramConfig{
		Enabled: true, BotToken: "bot-token", ChatID: testTelegramChatID, WebhookSecret: testTelegramSecret, APIURL: srv.URL,
	})
	return f
}

func settleButtonPress(settlementID uuid.UUID, chatID string) []byte {
	return []byte(`{"update_id":1,"callback_query":{"id":"cb-1","data":"settle:` + settlementID.String() +
		`","from":{"id":42,"username":"opsperson"},"message":{"message_id":7,"chat":{"id":` + chatID + `}}}}`)
}

var testBankDestination = json.RawMessage(`{"account_number":"0123456789","bank_code":"058","account_name":"Ada Obi","bank_name":"GTBank"}`)

func TestManualPayout_TerminalFailureGoesToTelegramThenButtonSettles(t *testing.T) {
	provider := &fakeSettlementProvider{name: "manualterminal", enabled: true, outcome: OutcomeRejectedTerminal, reason: "invalid account"}
	env := setupTestEnv(t, provider)
	defer env.Cleanup()
	tg := env.withTelegram(t)
	ctx := context.Background()

	sess := env.createDepositConfirmedSessionTo(t, decimal.NewFromInt(99000), decimal.NewFromInt(100), testBankDestination)
	st := env.waitForSettlementRow(t, sess.ID)
	env.runDispatchOnce(t)

	// The session's own payout_destination reached the provider.
	if string(provider.lastRequest.Destination) == "" || !strings.Contains(string(provider.lastRequest.Destination), "0123456789") {
		t.Fatalf("expected the session's payout_destination on the first attempt, got %s", provider.lastRequest.Destination)
	}

	st = env.waitForSettlementStatus(t, st.ID, StatusAwaitingManualPayout)
	if st.OpsPagedAt == nil {
		t.Fatal("expected ops_paged_at set on hand-off to manual payout")
	}
	// From the tenant's side the payout is still in progress.
	env.waitForSessionStatus(t, sess.ID, session.StatusSettling)

	sends := tg.byMethod("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("expected one manual-payout alert, got %d", len(sends))
	}
	text, _ := sends[0].Body["text"].(string)
	for _, want := range []string{"Manual Settlement Required", "0123456789", "GTBank", "98010.00", "invalid account"} {
		if !strings.Contains(text, want) {
			t.Fatalf("alert missing %q:\n%s", want, text)
		}
	}
	markup, _ := json.Marshal(sends[0].Body["reply_markup"])
	if !strings.Contains(string(markup), `"callback_data":"settle:`+st.ID.String()+`"`) {
		t.Fatalf("alert missing the settle button for %s: %s", st.ID, markup)
	}

	// A press from some other chat is refused, not acted on.
	if err := env.settlement.HandleTelegramUpdate(ctx, testTelegramSecret, settleButtonPress(st.ID, "999")); err != nil {
		t.Fatalf("foreign-chat press: %v", err)
	}
	if got, _ := env.settlement.GetSettlement(ctx, st.ID); got.Status != StatusAwaitingManualPayout {
		t.Fatalf("a press from another chat must not settle, got %s", got.Status)
	}
	// A wrong secret token is rejected outright.
	if err := env.settlement.HandleTelegramUpdate(ctx, "wrong", settleButtonPress(st.ID, testTelegramChatID)); !errors.Is(err, ErrInvalidTelegramSecret) {
		t.Fatalf("expected ErrInvalidTelegramSecret, got %v", err)
	}

	if err := env.settlement.HandleTelegramUpdate(ctx, testTelegramSecret, settleButtonPress(st.ID, testTelegramChatID)); err != nil {
		t.Fatalf("settle press: %v", err)
	}
	env.waitForSettlementStatus(t, st.ID, StatusSettled)
	env.waitForSessionStatus(t, sess.ID, session.StatusSettled)

	if len(tg.byMethod("editMessageReplyMarkup")) != 1 {
		t.Fatal("expected the alert's buttons to be cleared after settling")
	}

	// The manual attempt is the audit trail.
	var provName, status string
	var payload []byte
	if err := env.pool.QueryRow(ctx,
		`SELECT provider_name, status, provider_payload FROM settlement_attempts WHERE settlement_id = $1 ORDER BY attempt_number DESC LIMIT 1`, st.ID,
	).Scan(&provName, &status, &payload); err != nil {
		t.Fatalf("query manual attempt: %v", err)
	}
	if provName != ManualProviderName || status != "succeeded" || !strings.Contains(string(payload), "telegram:42 (@opsperson)") {
		t.Fatalf("unexpected manual attempt: %s %s %s", provName, status, payload)
	}

	// Ledger: the terminal attempt's claim was reversed, the manual payout
	// re-claimed it — tenant_payable nets to zero exactly once.
	l := ledger.New(env.pool)
	tid := env.tenantID
	if bal := env.balance(t, l, &tid, "tenant_payable", env.fiat, "fiat"); !bal.IsZero() {
		t.Fatalf("tenant_payable: expected 0 after manual settlement, got %s", bal)
	}
	if bal := env.balance(t, l, nil, "treasury_fiat_operating", env.fiat, "fiat"); !bal.Equal(decimal.NewFromInt(-98010)) {
		t.Fatalf("treasury_fiat_operating: expected -98010, got %s", bal)
	}

	// A second press is refused (told why), never a second settlement.
	answersBefore := len(tg.byMethod("answerCallbackQuery"))
	if err := env.settlement.HandleTelegramUpdate(ctx, testTelegramSecret, settleButtonPress(st.ID, testTelegramChatID)); err != nil {
		t.Fatalf("second press: %v", err)
	}
	answers := tg.byMethod("answerCallbackQuery")
	if len(answers) != answersBefore+1 || answers[len(answers)-1].Body["show_alert"] != true {
		t.Fatalf("expected the second press to be answered with a refusal alert, got %+v", answers)
	}
	if bal := env.balance(t, l, &tid, "tenant_payable", env.fiat, "fiat"); !bal.IsZero() {
		t.Fatalf("tenant_payable moved on a second press: %s", bal)
	}
}

func TestManualPayout_TelegramDownFallsBackToSettlementFailed(t *testing.T) {
	provider := &fakeSettlementProvider{name: "manualtgdown", enabled: true, outcome: OutcomeRejectedTerminal, reason: "invalid account"}
	env := setupTestEnv(t, provider)
	defer env.Cleanup()
	tg := env.withTelegram(t)
	tg.fail = true

	sess := env.createDepositConfirmedSessionTo(t, decimal.NewFromInt(99000), decimal.NewFromInt(100), testBankDestination)
	st := env.waitForSettlementRow(t, sess.ID)
	env.runDispatchOnce(t)

	env.waitForSettlementStatus(t, st.ID, StatusSettlementFailed)
	env.waitForSessionStatus(t, sess.ID, session.StatusSettlementFailed)
}

func TestManualPayout_RetryPayoutFromAwaitingManual(t *testing.T) {
	provider := &fakeSettlementProvider{name: "manualretry", enabled: true, outcome: OutcomeRejectedTerminal, reason: "invalid account"}
	env := setupTestEnv(t, provider)
	defer env.Cleanup()
	env.withTelegram(t)
	ctx := context.Background()

	sess := env.createDepositConfirmedSessionTo(t, decimal.NewFromInt(99000), decimal.NewFromInt(100), testBankDestination)
	st := env.waitForSettlementRow(t, sess.ID)
	env.runDispatchOnce(t)
	env.waitForSettlementStatus(t, st.ID, StatusAwaitingManualPayout)

	corrected := json.RawMessage(`{"account_number":"9999999999","bank_code":"044","account_name":"Ada Obi"}`)
	if _, err := env.settlement.RetryPayout(ctx, st.ID, uuid.New(), corrected, false); err != nil {
		t.Fatalf("retry payout from awaiting_manual_payout: %v", err)
	}
	provider.outcome, provider.ref = OutcomeAccepted, "ref-"+uuid.NewString()
	env.runDispatchOnce(t)
	env.waitForSettlementStatus(t, st.ID, StatusSettling)
	if !strings.Contains(string(provider.lastRequest.Destination), "9999999999") {
		t.Fatalf("expected the corrected destination on the retry, got %s", provider.lastRequest.Destination)
	}

	// The stale alert's button is now refused.
	if _, err := env.settlement.ManualSettle(ctx, st.ID, "test"); !errors.Is(err, ErrNotAwaitingManualPayout) {
		t.Fatalf("expected ErrNotAwaitingManualPayout once retried, got %v", err)
	}
}

func TestUncertainDispatch_HeldNotReversedThenConfirmedFailedRetry(t *testing.T) {
	provider := &fakeSettlementProvider{name: "uncertain", enabled: true, outcome: OutcomeUncertain, ref: "ref-" + uuid.NewString(), reason: "outcome unknown: timeout"}
	env := setupTestEnv(t, provider)
	defer env.Cleanup()
	tg := env.withTelegram(t)
	ctx := context.Background()
	l := ledger.New(env.pool)
	tid := env.tenantID

	sess := env.createDepositConfirmedSessionTo(t, decimal.NewFromInt(99000), decimal.NewFromInt(100), testBankDestination)
	st := env.waitForSettlementRow(t, sess.ID)
	env.runDispatchOnce(t)

	st = env.waitForSettlementStatus(t, st.ID, StatusSettling)
	if provider.calls != 1 {
		t.Fatalf("an uncertain outcome must never be auto-retried, got %d calls", provider.calls)
	}
	if st.OpsPagedAt == nil {
		t.Fatal("expected ops paged immediately on an uncertain dispatch")
	}
	var attemptStatus string
	if err := env.pool.QueryRow(ctx, `SELECT status FROM settlement_attempts WHERE settlement_id = $1 AND provider_reference = $2`, st.ID, provider.ref).Scan(&attemptStatus); err != nil {
		t.Fatalf("query uncertain attempt: %v", err)
	}
	if attemptStatus != "dispatched" {
		t.Fatalf("expected the uncertain attempt to stay dispatched, got %s", attemptStatus)
	}
	// Claim kept, not reversed: the money may be gone.
	if bal := env.balance(t, l, &tid, "tenant_payable", env.fiat, "fiat"); !bal.IsZero() {
		t.Fatalf("tenant_payable: expected the claim to stand (0), got %s", bal)
	}
	sends := tg.byMethod("sendMessage")
	if len(sends) != 1 || !strings.Contains(sends[0].Body["text"].(string), "Do NOT pay manually") || sends[0].Body["reply_markup"] != nil {
		t.Fatalf("expected one button-less review alert, got %+v", sends)
	}

	// Ops confirm with the provider it failed and retry.
	if _, err := env.settlement.RetryPayout(ctx, st.ID, uuid.New(), nil, true); err != nil {
		t.Fatalf("confirmed-failed retry: %v", err)
	}
	if err := env.pool.QueryRow(ctx, `SELECT status FROM settlement_attempts WHERE settlement_id = $1 AND provider_reference = $2`, st.ID, provider.ref).Scan(&attemptStatus); err != nil {
		t.Fatalf("query uncertain attempt: %v", err)
	}
	if attemptStatus != "failed_bucket3" {
		t.Fatalf("expected the confirmed-failed attempt closed as failed_bucket3, got %s", attemptStatus)
	}

	// A late success webhook for the closed attempt is a no-op.
	body := []byte(`{"event_type":"payout.succeeded","reference_id":"` + provider.ref + `"}`)
	if err := env.settlement.HandleSettlementWebhook(ctx, provider.name, body, sigHeader(ComputeWebhookSignature(provider.webhookSecret(), body))); err != nil {
		t.Fatalf("late webhook: %v", err)
	}

	provider.outcome, provider.ref = OutcomeAccepted, "ref-"+uuid.NewString()
	env.runDispatchOnce(t)
	env.waitForSettlementStatus(t, st.ID, StatusSettling)
	if !strings.Contains(string(provider.lastRequest.Destination), "0123456789") {
		t.Fatalf("expected the previous attempt's destination to carry over, got %s", provider.lastRequest.Destination)
	}

	// Exactly one live claim — the closed attempt's was reversed, so the
	// new attempt didn't double-debit.
	if bal := env.balance(t, l, &tid, "tenant_payable", env.fiat, "fiat"); !bal.IsZero() {
		t.Fatalf("tenant_payable: expected 0 (one live claim), got %s", bal)
	}
}

func TestWebhook_DeclinedPagesOpsWithoutResolving(t *testing.T) {
	provider := &fakeSettlementProvider{name: "declined", enabled: true, outcome: OutcomeAccepted, ref: "ref-" + uuid.NewString()}
	// A second provider so the eventual reversal fails over immediately
	// instead of sleeping RetryBackoff before retrying the only one.
	backup := &fakeSettlementProvider{name: "declinedbackup", enabled: true, outcome: OutcomeAccepted, ref: "ref-" + uuid.NewString()}
	env := setupTestEnv(t, provider, backup)
	defer env.Cleanup()
	tg := env.withTelegram(t)
	ctx := context.Background()

	sess := env.createDepositConfirmedSessionTo(t, decimal.NewFromInt(99000), decimal.NewFromInt(100), testBankDestination)
	st := env.waitForSettlementRow(t, sess.ID)
	env.runDispatchOnce(t)
	env.waitForSettlementStatus(t, st.ID, StatusSettling)

	provider.event = &webhookEvent{Kind: webhookDeclined, ProviderReference: provider.ref, Reason: "Do not honor (code 05)"}
	body := []byte(`{"declined":true}`)
	header := sigHeader(ComputeWebhookSignature(provider.webhookSecret(), body))
	for range 2 { // the redelivery must not re-alert
		if err := env.settlement.HandleSettlementWebhook(ctx, provider.name, body, header); err != nil {
			t.Fatalf("declined webhook: %v", err)
		}
	}

	got, err := env.settlement.GetSettlement(ctx, st.ID)
	if err != nil {
		t.Fatalf("get settlement: %v", err)
	}
	if got.Status != StatusSettling || got.OpsPagedAt == nil {
		t.Fatalf("expected still settling with ops paged, got %s paged=%v", got.Status, got.OpsPagedAt != nil)
	}
	if provider.calls != 1 {
		t.Fatalf("a decline must not trigger a retry, got %d calls", provider.calls)
	}
	if sends := tg.byMethod("sendMessage"); len(sends) != 1 || !strings.Contains(sends[0].Body["text"].(string), "Awaiting reversal") {
		t.Fatalf("expected exactly one review alert, got %+v", sends)
	}

	// The eventual reversal means nothing is in flight: a bucket-2 failure,
	// reversed in the ledger and failed over to the next provider.
	provider.event = &webhookEvent{Kind: webhookReversed, ProviderReference: provider.ref, Reason: "Reversed: declined"}
	if err := env.settlement.HandleSettlementWebhook(ctx, provider.name, body, header); err != nil {
		t.Fatalf("reversed webhook: %v", err)
	}
	if provider.calls != 1 || backup.calls != 1 {
		t.Fatalf("expected the reversal to fail over to the backup, got provider=%d backup=%d", provider.calls, backup.calls)
	}
	l := ledger.New(env.pool)
	tid := env.tenantID
	if bal := env.balance(t, l, &tid, "tenant_payable", env.fiat, "fiat"); !bal.IsZero() {
		t.Fatalf("tenant_payable: expected one live claim (0) after reverse + re-dispatch, got %s", bal)
	}
}
