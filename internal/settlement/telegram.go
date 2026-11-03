package settlement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// TelegramConfig configures the ops Telegram group settlement falls back to
// when automatic payout fails (ARCHITECTURE.md §8 "Manual payout fallback").
// WebhookSecret is the secret_token registered with Telegram's setWebhook;
// without it the "Settlement completed" button can't be accepted, so alerts
// still go out but manual settles are refused.
type TelegramConfig struct {
	Enabled       bool
	BotToken      string
	ChatID        string
	WebhookSecret string
	// APIURL defaults to https://api.telegram.org — overridable for tests.
	APIURL string
}

const (
	telegramDefaultAPIURL = "https://api.telegram.org"
	telegramTimeout       = 10 * time.Second
	// telegramSettleAction prefixes the manual-settle button's callback_data
	// (Telegram caps callback_data at 64 bytes; "settle:" + a UUID is 43).
	telegramSettleAction = "settle:"
	// Bot API caps copy_text at 256 characters.
	telegramCopyTextMax = 256
)

type telegramNotifier struct {
	cfg    TelegramConfig
	client *http.Client
}

// newTelegramNotifier returns nil unless alerts are enabled and have a bot
// and a chat to post to — every caller treats nil as "no Telegram
// fallback", so the pipeline behaves exactly as it did without it.
func newTelegramNotifier(cfg TelegramConfig) *telegramNotifier {
	if !cfg.Enabled || cfg.BotToken == "" || cfg.ChatID == "" {
		return nil
	}
	if cfg.APIURL == "" {
		cfg.APIURL = telegramDefaultAPIURL
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	return &telegramNotifier{cfg: cfg, client: &http.Client{Timeout: telegramTimeout}}
}

type telegramButton struct {
	Text         string            `json:"text"`
	CallbackData string            `json:"callback_data,omitempty"`
	CopyText     *telegramCopyText `json:"copy_text,omitempty"`
}

// telegramCopyText makes a button copy its text on tap (Bot API 7.11+).
type telegramCopyText struct {
	Text string `json:"text"`
}

type telegramKeyboard struct {
	InlineKeyboard [][]telegramButton `json:"inline_keyboard"`
}

func copyButton(label, value string) telegramButton {
	if len(value) > telegramCopyTextMax {
		value = value[:telegramCopyTextMax]
	}
	return telegramButton{Text: label, CopyText: &telegramCopyText{Text: value}}
}

func (t *telegramNotifier) call(ctx context.Context, method string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.APIURL+"/bot"+t.cfg.BotToken+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		// The URL embeds the bot token — never let it reach a log line.
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram %s: HTTP %d: %s", method, resp.StatusCode, body)
	}
	return nil
}

func (t *telegramNotifier) sendMessage(ctx context.Context, text string, keyboard *telegramKeyboard) error {
	payload := map[string]any{"chat_id": t.cfg.ChatID, "text": text, "parse_mode": "HTML"}
	if keyboard != nil {
		payload["reply_markup"] = keyboard
	}
	return t.call(ctx, "sendMessage", payload)
}

func (t *telegramNotifier) answerCallback(ctx context.Context, callbackID, text string, showAlert bool) error {
	return t.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text, "show_alert": showAlert})
}

// clearButtons removes the inline keyboard from an alert once it's handled,
// so the same button can't be pressed again from that message.
func (t *telegramNotifier) clearButtons(ctx context.Context, chatID int64, messageID int) error {
	return t.call(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": chatID, "message_id": messageID})
}

func esc(s string) string { return html.EscapeString(s) }

// payoutAmount is the raw two-decimal amount (no thousands separators) so it
// pastes cleanly into a banking app.
func payoutAmount(st *Settlement) string { return st.TenantPayableAmount.StringFixed(2) }

func manualPayoutMessage(st *Settlement, dest bankDestination, reason string) (string, *telegramKeyboard) {
	bank := dest.BankName
	if bank == "" {
		bank = dest.BankCode
	}
	text := fmt.Sprintf(`<b>Manual Settlement Required</b>

<b>Session:</b> <code>%s</code>
<b>Amount (%s):</b> <code>%s</code>
<b>Account:</b> <code>%s</code>
<b>Bank:</b> <code>%s</code>
<b>Name:</b> %s

<b>Error:</b> %s

Use the Copy buttons below, then mark the settlement completed after manual payment.`,
		esc(st.SessionID.String()), esc(st.FiatCurrency), esc(payoutAmount(st)),
		esc(dest.AccountNumber), esc(bank), esc(dest.AccountName), esc(reason))

	keyboard := &telegramKeyboard{InlineKeyboard: [][]telegramButton{
		{copyButton("Copy amount", payoutAmount(st)), copyButton("Copy account", dest.AccountNumber)},
		{copyButton("Copy bank", bank)},
		{{Text: "Settlement completed", CallbackData: telegramSettleAction + st.ID.String()}},
	}}
	return text, keyboard
}

// reviewMessage deliberately carries no manual-settle button: the transfer
// may still land, and paying by hand meanwhile would pay twice.
func reviewMessage(st *Settlement, reference, reason string) string {
	return fmt.Sprintf(`<b>Settlement Needs Review</b>

<b>Session:</b> <code>%s</code>
<b>Amount:</b> %s %s
<b>Reference:</b> <code>%s</code>

<b>Status:</b> %s

<b>Do NOT pay manually yet.</b> The transfer may still land. Look up the reference in the provider dashboard first.`,
		esc(st.SessionID.String()), esc(st.FiatCurrency), esc(payoutAmount(st)), esc(reference), esc(reason))
}

func reversalMessage(st *Settlement, reason string) string {
	return fmt.Sprintf(`<b>Settlement Reversed</b>

<b>Session:</b> <code>%s</code>
<b>Amount:</b> %s %s

<b>Reason:</b> %s

<b>Action Required:</b> Investigate and resolve via the settlement reversals queue.`,
		esc(st.SessionID.String()), esc(st.FiatCurrency), esc(payoutAmount(st)), esc(reason))
}

func manualSettledMessage(st *Settlement, by string) string {
	return fmt.Sprintf("<b>Manual settlement completed</b>\n\n<b>Session:</b> <code>%s</code>\n<b>Marked by:</b> %s",
		esc(st.SessionID.String()), esc(by))
}

// alertReview and alertReversal are best-effort pages: a Telegram outage
// must never fail the settlement transition that triggered them (ops_paged_at
// and the ops queue still surface the row).
func (s *Store) alertReview(ctx context.Context, st *Settlement, reference, reason string) {
	if s.telegram == nil {
		return
	}
	if err := s.telegram.sendMessage(ctx, reviewMessage(st, reference, reason), nil); err != nil {
		log.Printf("settlement: telegram review alert for %s: %v", st.ID, err)
	}
}

func (s *Store) alertReversal(ctx context.Context, st *Settlement, reason string) {
	if s.telegram == nil {
		return
	}
	if err := s.telegram.sendMessage(ctx, reversalMessage(st, reason), nil); err != nil {
		log.Printf("settlement: telegram reversal alert for %s: %v", st.ID, err)
	}
}
