package settlement

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// HyparrowProviderName is the registered name of the Hyparrow NGN payout
// adapter — the provider_name a corridor's settlement binding uses.
const HyparrowProviderName = "hyparrow"

// HyparrowConfig configures the Hyparrow money-transfer adapter
// (https://docs.hyparrow.com/money-transfer.md). The sender fields are
// required on every transfer: they describe 2Settle as the business sending
// the payout, not the crypto payer.
type HyparrowConfig struct {
	Enabled          bool
	BaseURL          string
	APIKey           string
	APISecret        string
	WebhookSecret    string
	SenderPhone      string
	SenderEmail      string
	SenderLastname   string
	SenderOthernames string
	Timeout          time.Duration
}

const (
	hyparrowDefaultTimeout = 30 * time.Second
	// Hyparrow re-signs every delivery attempt, so a genuine callback is
	// always fresh — anything older is a replay.
	hyparrowWebhookTolerance        = 300 * time.Second
	hyparrowSignatureHeader         = "X-Hyparrow-Signature-256"
	hyparrowTimestampHeader         = "X-Hyparrow-Timestamp"
	hyparrowSupportedFiatCurrency   = "NGN"
	hyparrowNarrationSessionIDChars = 8
)

// bankDestination is the NGN bank-transfer shape of a payout destination —
// what an NGN corridor's required_destination_fields should list
// (account_number, bank_code, account_name). bank_name is optional and only
// used to make the manual-payout alert readable.
type bankDestination struct {
	AccountNumber string `json:"account_number"`
	BankCode      string `json:"bank_code"`
	AccountName   string `json:"account_name"`
	BankName      string `json:"bank_name"`
}

func parseBankDestination(raw json.RawMessage) (bankDestination, error) {
	var d bankDestination
	if len(raw) == 0 {
		return d, fmt.Errorf("payout destination is empty")
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return d, fmt.Errorf("payout destination is not a bank-details object: %w", err)
	}
	d.AccountNumber = strings.TrimSpace(d.AccountNumber)
	d.BankCode = strings.TrimSpace(d.BankCode)
	d.AccountName = strings.TrimSpace(d.AccountName)
	d.BankName = strings.TrimSpace(d.BankName)
	return d, nil
}

// hyparrowClientReference is the clientReference sent with an attempt.
// Hyparrow treats it as a permanent idempotency key — resending the same
// reference never pays twice — which is what makes a lost response
// recoverable: look the transfer up by the reference generated before the
// call. It is per attempt, not per session: v2 only starts a new attempt
// once the previous one is known not to be in flight (rejected or
// reversed), and a replay of a reversed reference would just return the
// reversal instead of paying.
func hyparrowClientReference(sessionID uuid.UUID, attemptNumber int) string {
	return fmt.Sprintf("2S-%s-%d", sessionID, attemptNumber)
}

// hyparrowKobo converts a naira amount to Hyparrow's integer-kobo string.
func hyparrowKobo(amount decimal.Decimal) string {
	return amount.Mul(decimal.NewFromInt(100)).Round(0).StringFixed(0)
}

type hyparrowProvider struct {
	cfg    HyparrowConfig
	client *http.Client
	now    func() time.Time
}

func newHyparrowProvider(cfg HyparrowConfig) *hyparrowProvider {
	if cfg.Timeout <= 0 {
		cfg.Timeout = hyparrowDefaultTimeout
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &hyparrowProvider{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}, now: time.Now}
}

func (p *hyparrowProvider) Name() string { return HyparrowProviderName }

// IsEnabled requires the credentials plus every sender field Hyparrow
// demands, so a half-configured adapter is skipped instead of failing every
// dispatch.
func (p *hyparrowProvider) IsEnabled() bool {
	c := p.cfg
	return c.Enabled && c.BaseURL != "" && c.APIKey != "" && c.APISecret != "" &&
		c.SenderPhone != "" && c.SenderEmail != "" && c.SenderLastname != "" && c.SenderOthernames != ""
}

// Dispatch never reports a rejection unless the money is definitely not in
// flight: a rejection makes dispatch.go reverse the ledger claim and either
// retry with a new reference or hand the payout to a human, both of which
// would pay twice if the transfer had actually gone out. An ambiguous
// response is looked up by reference, and if that can't settle it either,
// reported as OutcomeUncertain.
func (p *hyparrowProvider) Dispatch(ctx context.Context, req PayoutRequest) (PayoutResult, error) {
	if req.FiatCurrency != hyparrowSupportedFiatCurrency {
		return PayoutResult{Outcome: OutcomeRejectedTerminal, FailureReason: fmt.Sprintf("hyparrow only pays %s, got %s", hyparrowSupportedFiatCurrency, req.FiatCurrency)}, nil
	}
	dest, err := parseBankDestination(req.Destination)
	if err != nil {
		return PayoutResult{Outcome: OutcomeRejectedTerminal, FailureReason: err.Error()}, nil
	}
	if dest.AccountNumber == "" || dest.BankCode == "" {
		return PayoutResult{Outcome: OutcomeRejectedTerminal, FailureReason: "payout destination is missing account_number or bank_code"}, nil
	}

	// Hyparrow expects the bank's registered name as recipientName.
	accountName, err := p.resolveAccount(ctx, dest.AccountNumber, dest.BankCode)
	if err != nil {
		return PayoutResult{Outcome: OutcomeRejectedRetryable, FailureReason: err.Error()}, nil
	}
	if dest.AccountName != "" && !strings.EqualFold(accountName, dest.AccountName) {
		log.Printf("settlement: hyparrow name mismatch for session %s: destination %q, bank %q", req.SessionID, dest.AccountName, accountName)
	}

	ref := hyparrowClientReference(req.SessionID, req.AttemptNumber)
	outcome := p.send(ctx, hyparrowSendRequest{
		Amount:            hyparrowKobo(req.Amount),
		RecipientAccount:  dest.AccountNumber,
		RecipientBankCode: dest.BankCode,
		RecipientName:     accountName,
		SenderPhone:       p.cfg.SenderPhone,
		SenderEmail:       p.cfg.SenderEmail,
		SenderLastname:    p.cfg.SenderLastname,
		SenderOthernames:  p.cfg.SenderOthernames,
		Narration:         "2Settle payout " + req.SessionID.String()[:hyparrowNarrationSessionIDChars],
		ClientReference:   ref,
	})

	switch outcome.kind {
	case hyparrowAccepted:
		return PayoutResult{Outcome: OutcomeAccepted, ProviderReference: ref}, nil
	case hyparrowRejected:
		return PayoutResult{Outcome: OutcomeRejectedRetryable, ProviderReference: ref, FailureReason: outcome.message}, nil
	}

	// Response lost — ask Hyparrow by the reference generated before sending.
	status, found, err := p.getStatus(ctx, ref)
	if err == nil && found {
		if status == "reversed" {
			return PayoutResult{Outcome: OutcomeRejectedRetryable, ProviderReference: ref, FailureReason: "transfer was reversed (recovered after: " + outcome.message + ")"}, nil
		}
		return PayoutResult{Outcome: OutcomeAccepted, ProviderReference: ref}, nil
	}
	// Still can't tell (not found yet, or the lookup failed too). Hold
	// rather than risk a second payout.
	return PayoutResult{Outcome: OutcomeUncertain, ProviderReference: ref, FailureReason: "outcome unknown: " + outcome.message}, nil
}

type hyparrowSendRequest struct {
	Amount            string `json:"amount"`
	RecipientAccount  string `json:"recipientAccount"`
	RecipientBankCode string `json:"recipientBankCode"`
	RecipientName     string `json:"recipientName"`
	SenderPhone       string `json:"senderPhone"`
	SenderEmail       string `json:"senderEmail"`
	SenderLastname    string `json:"senderLastname"`
	SenderOthernames  string `json:"senderOthernames"`
	Narration         string `json:"narration,omitempty"`
	ClientReference   string `json:"clientReference"`
}

type hyparrowResponseBody struct {
	Success *bool  `json:"success"`
	Message string `json:"message"`
	Error   string `json:"error"`
	Status  string `json:"status"`
	Data    struct {
		AccountName  string `json:"accountName"`
		TransferCode string `json:"TransferCode"`
		Status       string `json:"Status"`
		Transaction  struct {
			SettlementStatus string `json:"settlementStatus"`
		} `json:"transaction"`
	} `json:"data"`
}

func (b hyparrowResponseBody) errorText(status int) string {
	if b.Error != "" {
		return b.Error
	}
	if b.Message != "" {
		return b.Message
	}
	return fmt.Sprintf("HTTP %d", status)
}

type hyparrowSendKind int

const (
	// hyparrowAccepted: 200, or 202 when the bank network didn't answer.
	hyparrowAccepted hyparrowSendKind = iota
	// hyparrowRejected: definitely not sent, or sent and already reversed.
	hyparrowRejected
	// hyparrowUnknown: timeout, dropped connection or unexplained 5xx.
	hyparrowUnknown
)

type hyparrowSendOutcome struct {
	kind    hyparrowSendKind
	message string
}

func (p *hyparrowProvider) send(ctx context.Context, req hyparrowSendRequest) hyparrowSendOutcome {
	status, body, err := p.do(ctx, http.MethodPost, "/money-transfer/send", req)
	if err != nil {
		return hyparrowSendOutcome{kind: hyparrowUnknown, message: "no response from hyparrow: " + err.Error()}
	}

	if status == http.StatusOK || status == http.StatusAccepted {
		// An idempotent replay returns the original transfer as it stands —
		// if that one was rejected and reversed, nothing is in flight.
		if body.Data.Status == "reversed" {
			return hyparrowSendOutcome{kind: hyparrowRejected, message: "transfer was rejected and reversed"}
		}
		return hyparrowSendOutcome{kind: hyparrowAccepted}
	}

	msg := body.errorText(status)
	// 4xx: validation, insufficient balance, reference conflict, or a
	// network rejection Hyparrow already reversed — nothing in flight.
	// 5xx with status "reversed": the request never left Hyparrow.
	if (status >= 400 && status < 500) || body.Status == "reversed" {
		return hyparrowSendOutcome{kind: hyparrowRejected, message: msg}
	}
	return hyparrowSendOutcome{kind: hyparrowUnknown, message: fmt.Sprintf("hyparrow returned HTTP %d: %s", status, msg)}
}

func (p *hyparrowProvider) resolveAccount(ctx context.Context, accountNumber, bankCode string) (string, error) {
	status, body, err := p.do(ctx, http.MethodPost, "/money-transfer/account-inquiry", map[string]string{
		"accountNumber": accountNumber,
		"bankCode":      bankCode,
	})
	if err != nil {
		return "", fmt.Errorf("hyparrow account inquiry: %w", err)
	}
	if status >= 200 && status < 300 && (body.Success == nil || *body.Success) && body.Data.AccountName != "" {
		return body.Data.AccountName, nil
	}
	return "", fmt.Errorf("hyparrow account inquiry failed: %s", body.errorText(status))
}

// getStatus looks a transfer up by clientReference. found is false on 404.
func (p *hyparrowProvider) getStatus(ctx context.Context, ref string) (settlementStatus string, found bool, err error) {
	status, body, err := p.do(ctx, http.MethodGet, "/money-transfer/status?reference="+url.QueryEscape(ref), nil)
	if err != nil {
		return "", false, err
	}
	if status == http.StatusNotFound {
		return "", false, nil
	}
	if status == http.StatusOK && body.Data.Transaction.SettlementStatus != "" {
		return body.Data.Transaction.SettlementStatus, true, nil
	}
	return "", false, fmt.Errorf("hyparrow status lookup: %s", body.errorText(status))
}

// do sends one request. A non-JSON body (gateways in front of Hyparrow can
// answer with HTML) is treated as empty rather than as an error, so the
// status code alone still classifies the response.
func (p *hyparrowProvider) do(ctx context.Context, method, path string, payload any) (int, hyparrowResponseBody, error) {
	var reqBody io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, hyparrowResponseBody{}, err
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.BaseURL+path, reqBody)
	if err != nil {
		return 0, hyparrowResponseBody{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-API-Key", p.cfg.APIKey)
	req.Header.Set("X-API-Secret", p.cfg.APISecret)

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, hyparrowResponseBody{}, err
	}
	defer resp.Body.Close()

	var body hyparrowResponseBody
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, hyparrowResponseBody{}, err
	}
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body, nil
}

// ComputeHyparrowSignature is Hyparrow's webhook signature: hex
// HMAC-SHA256 of "<timestamp>.<raw body>" under the webhook secret.
func ComputeHyparrowSignature(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyHyparrowSignature checks X-Hyparrow-Signature-256 against the
// timestamped body and rejects anything outside hyparrowWebhookTolerance,
// so a captured delivery can't be replayed later.
func verifyHyparrowSignature(secret, timestamp, signature string, body []byte, now time.Time) error {
	if timestamp == "" || signature == "" {
		return ErrInvalidWebhookSignature
	}
	ts, err := strconv.ParseFloat(timestamp, 64)
	if err != nil || math.IsNaN(ts) || math.IsInf(ts, 0) {
		return ErrInvalidWebhookSignature
	}
	age := now.Sub(time.Unix(0, int64(ts*float64(time.Second))))
	if age > hyparrowWebhookTolerance || age < -hyparrowWebhookTolerance {
		return ErrInvalidWebhookSignature
	}
	if !hmac.Equal([]byte(ComputeHyparrowSignature(secret, timestamp, body)), []byte(signature)) {
		return ErrInvalidWebhookSignature
	}
	return nil
}

// hyparrowWebhook is the envelope shared by Hyparrow's transfer webhooks
// (transfer.settled / transfer.failed / transfer.reversed).
type hyparrowWebhook struct {
	Event string `json:"event"`
	Data  struct {
		ClientReference string `json:"clientReference"`
		Reason          string `json:"reason"`
		Provider        struct {
			TransactionResponseCode string `json:"transactionResponseCode"`
			ResponseMessage         string `json:"responseMessage"`
		} `json:"provider"`
	} `json:"data"`
}

func (p *hyparrowProvider) parseWebhook(header http.Header, body []byte) (webhookEvent, error) {
	if p.cfg.WebhookSecret == "" {
		return webhookEvent{}, fmt.Errorf("%w: %s has no webhook secret configured", ErrUnknownProvider, HyparrowProviderName)
	}
	if err := verifyHyparrowSignature(p.cfg.WebhookSecret, header.Get(hyparrowTimestampHeader), header.Get(hyparrowSignatureHeader), body, p.now()); err != nil {
		return webhookEvent{}, err
	}

	var payload hyparrowWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		return webhookEvent{}, fmt.Errorf("settlement: parse hyparrow webhook: %w", err)
	}
	ref := payload.Data.ClientReference
	if ref == "" {
		// Account-level events (wallet.funded, test.webhook, ...) carry no
		// transfer reference and don't affect settlement.
		return webhookEvent{Kind: webhookIgnored}, nil
	}

	switch payload.Event {
	case "transfer.settled":
		return webhookEvent{Kind: webhookSucceeded, ProviderReference: ref}, nil
	case "transfer.failed":
		reason := payload.Data.Provider.ResponseMessage
		if reason == "" {
			reason = "Declined"
		}
		if code := payload.Data.Provider.TransactionResponseCode; code != "" {
			reason += " (code " + code + ")"
		}
		return webhookEvent{Kind: webhookDeclined, ProviderReference: ref, Reason: reason}, nil
	case "transfer.reversed":
		reason := payload.Data.Reason
		if reason == "" {
			reason = "unknown reason"
		}
		return webhookEvent{Kind: webhookReversed, ProviderReference: ref, Reason: "Reversed: " + reason}, nil
	default:
		return webhookEvent{Kind: webhookIgnored}, nil
	}
}
