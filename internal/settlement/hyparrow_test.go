package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// fakeHyparrow is an httptest stand-in for Hyparrow's money-transfer API.
// sendStatus/sendBody shape /send's reply (sendStatus 0 drops the
// connection); lookupStatus/lookupBody shape /status's.
type fakeHyparrow struct {
	t            *testing.T
	sendStatus   int
	sendBody     string
	lookupStatus int
	lookupBody   string

	mu       sync.Mutex
	sent     []hyparrowSendRequest
	lookedUp []string
}

func (f *fakeHyparrow) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-API-Key") != "key" || r.Header.Get("X-API-Secret") != "secret" {
		f.t.Errorf("missing/incorrect hyparrow auth headers on %s", r.URL.Path)
	}
	switch r.URL.Path {
	case "/money-transfer/account-inquiry":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["accountNumber"] == "0000000000" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"error":"Invalid account"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"accountName":"ADA OBI"}}`))
	case "/money-transfer/send":
		var req hyparrowSendRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.sent = append(f.sent, req)
		f.mu.Unlock()
		if f.sendStatus == 0 {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		w.WriteHeader(f.sendStatus)
		_, _ = w.Write([]byte(f.sendBody))
	case "/money-transfer/status":
		f.mu.Lock()
		f.lookedUp = append(f.lookedUp, r.URL.Query().Get("reference"))
		f.mu.Unlock()
		w.WriteHeader(f.lookupStatus)
		_, _ = w.Write([]byte(f.lookupBody))
	default:
		f.t.Errorf("unexpected hyparrow path %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func testHyparrowConfig(baseURL string) HyparrowConfig {
	return HyparrowConfig{
		Enabled: true, BaseURL: baseURL, APIKey: "key", APISecret: "secret", WebhookSecret: "whsec",
		SenderPhone: "+2348000000000", SenderEmail: "ops@2settle.test", SenderLastname: "Settle", SenderOthernames: "Two",
		Timeout: 2 * time.Second,
	}
}

func newTestHyparrow(t *testing.T, f *fakeHyparrow) *hyparrowProvider {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return newHyparrowProvider(testHyparrowConfig(srv.URL))
}

var ngnDestination = json.RawMessage(`{"account_number":"0123456789","bank_code":"058","account_name":"Ada Obi","bank_name":"GTBank"}`)

func ngnPayout(dest json.RawMessage) PayoutRequest {
	return PayoutRequest{
		SessionID:     uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		AttemptNumber: 2,
		FiatCurrency:  "NGN",
		Amount:        decimal.RequireFromString("98010.50"),
		Destination:   dest,
	}
}

func TestHyparrowDispatch_Accepted(t *testing.T) {
	f := &fakeHyparrow{sendStatus: http.StatusOK, sendBody: `{"success":true,"data":{"TransferCode":"TRF1","Status":"pending"}}`}
	p := newTestHyparrow(t, f)

	res, err := p.Dispatch(context.Background(), ngnPayout(ngnDestination))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	wantRef := "2S-11111111-2222-3333-4444-555555555555-2"
	if res.Outcome != OutcomeAccepted || res.ProviderReference != wantRef {
		t.Fatalf("expected accepted with ref %s, got %+v", wantRef, res)
	}
	if len(f.sent) != 1 {
		t.Fatalf("expected one send, got %d", len(f.sent))
	}
	got := f.sent[0]
	if got.Amount != "9801050" {
		t.Fatalf("expected amount in kobo 9801050, got %s", got.Amount)
	}
	if got.RecipientName != "ADA OBI" {
		t.Fatalf("expected the bank-resolved recipient name, got %q", got.RecipientName)
	}
	if got.RecipientAccount != "0123456789" || got.RecipientBankCode != "058" || got.ClientReference != wantRef {
		t.Fatalf("unexpected send body: %+v", got)
	}
	if got.SenderEmail != "ops@2settle.test" || got.SenderOthernames != "Two" {
		t.Fatalf("sender details not sent: %+v", got)
	}
}

func TestHyparrowDispatch_Outcomes(t *testing.T) {
	cases := []struct {
		name         string
		fake         *fakeHyparrow
		dest         json.RawMessage
		currency     string
		want         DispatchOutcome
		wantSends    int
		wantLookedUp bool
	}{
		{name: "4xx rejection is not in flight", fake: &fakeHyparrow{sendStatus: http.StatusBadRequest, sendBody: `{"error":"Insufficient balance"}`}, want: OutcomeRejectedRetryable, wantSends: 1},
		{name: "replay of a reversed transfer", fake: &fakeHyparrow{sendStatus: http.StatusOK, sendBody: `{"data":{"Status":"reversed"}}`}, want: OutcomeRejectedRetryable, wantSends: 1},
		{name: "5xx marked reversed never left hyparrow", fake: &fakeHyparrow{sendStatus: http.StatusBadGateway, sendBody: `{"status":"reversed"}`}, want: OutcomeRejectedRetryable, wantSends: 1},
		{name: "5xx recovered by lookup as pending", fake: &fakeHyparrow{sendStatus: http.StatusInternalServerError, lookupStatus: http.StatusOK, lookupBody: `{"data":{"transaction":{"settlementStatus":"pending"}}}`}, want: OutcomeAccepted, wantSends: 1, wantLookedUp: true},
		{name: "5xx recovered by lookup as reversed", fake: &fakeHyparrow{sendStatus: http.StatusInternalServerError, lookupStatus: http.StatusOK, lookupBody: `{"data":{"transaction":{"settlementStatus":"reversed"}}}`}, want: OutcomeRejectedRetryable, wantSends: 1, wantLookedUp: true},
		{name: "5xx and lookup finds nothing yet", fake: &fakeHyparrow{sendStatus: http.StatusInternalServerError, lookupStatus: http.StatusNotFound}, want: OutcomeUncertain, wantSends: 1, wantLookedUp: true},
		{name: "dropped connection and lookup errors", fake: &fakeHyparrow{sendStatus: 0, lookupStatus: http.StatusInternalServerError}, want: OutcomeUncertain, wantSends: 1, wantLookedUp: true},
		{name: "unresolvable account", fake: &fakeHyparrow{}, dest: json.RawMessage(`{"account_number":"0000000000","bank_code":"058"}`), want: OutcomeRejectedRetryable},
		{name: "missing bank code", fake: &fakeHyparrow{}, dest: json.RawMessage(`{"account_number":"0123456789"}`), want: OutcomeRejectedTerminal},
		{name: "empty destination", fake: &fakeHyparrow{}, dest: json.RawMessage(`{}`), want: OutcomeRejectedTerminal},
		{name: "non-NGN currency", fake: &fakeHyparrow{}, currency: "GHS", want: OutcomeRejectedTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fake
			p := newTestHyparrow(t, f)
			req := ngnPayout(ngnDestination)
			if tc.dest != nil {
				req.Destination = tc.dest
			}
			if tc.currency != "" {
				req.FiatCurrency = tc.currency
			}
			res, err := p.Dispatch(context.Background(), req)
			if err != nil {
				t.Fatalf("dispatch: %v", err)
			}
			if res.Outcome != tc.want {
				t.Fatalf("expected %s, got %+v", tc.want, res)
			}
			if len(f.sent) != tc.wantSends {
				t.Fatalf("expected %d sends, got %d", tc.wantSends, len(f.sent))
			}
			if tc.wantLookedUp && (len(f.lookedUp) != 1 || f.lookedUp[0] != hyparrowClientReference(req.SessionID, req.AttemptNumber)) {
				t.Fatalf("expected one status lookup by client reference, got %v", f.lookedUp)
			}
			if !tc.wantLookedUp && len(f.lookedUp) != 0 {
				t.Fatalf("expected no status lookup, got %v", f.lookedUp)
			}
			if tc.want == OutcomeUncertain && res.ProviderReference == "" {
				t.Fatal("an uncertain outcome must carry the reference so a webhook can still resolve it")
			}
		})
	}
}

func TestHyparrowIsEnabled_RequiresSenderDetails(t *testing.T) {
	cfg := testHyparrowConfig("https://example.test")
	if !newHyparrowProvider(cfg).IsEnabled() {
		t.Fatal("fully-configured hyparrow should be enabled")
	}
	cfg.SenderLastname = ""
	if newHyparrowProvider(cfg).IsEnabled() {
		t.Fatal("hyparrow without every sender field must stay disabled")
	}
	cfg = testHyparrowConfig("https://example.test")
	cfg.Enabled = false
	if newHyparrowProvider(cfg).IsEnabled() {
		t.Fatal("HYPARROW_ENABLED=false must disable it")
	}
}

func signedHyparrowHeader(secret string, ts time.Time, body []byte) http.Header {
	stamp := strconv.FormatInt(ts.Unix(), 10)
	h := http.Header{}
	h.Set(hyparrowTimestampHeader, stamp)
	h.Set(hyparrowSignatureHeader, ComputeHyparrowSignature(secret, stamp, body))
	return h
}

func TestHyparrowParseWebhook(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := newHyparrowProvider(testHyparrowConfig("https://example.test"))
	p.now = func() time.Time { return now }

	cases := []struct {
		name       string
		body       string
		wantKind   webhookEventKind
		wantReason string
	}{
		{"settled", `{"event":"transfer.settled","data":{"clientReference":"2S-x-1"}}`, webhookSucceeded, ""},
		{"failed is a decline still in flight", `{"event":"transfer.failed","data":{"clientReference":"2S-x-1","provider":{"responseMessage":"Do not honor","transactionResponseCode":"05"}}}`, webhookDeclined, "Do not honor (code 05)"},
		{"reversed", `{"event":"transfer.reversed","data":{"clientReference":"2S-x-1","reason":"Beneficiary bank unavailable"}}`, webhookReversed, "Reversed: Beneficiary bank unavailable"},
		{"account event without reference", `{"event":"wallet.funded","data":{}}`, webhookIgnored, ""},
		{"unknown transfer event", `{"event":"transfer.queued","data":{"clientReference":"2S-x-1"}}`, webhookIgnored, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			ev, err := p.parseWebhook(signedHyparrowHeader("whsec", now, body), body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if ev.Kind != tc.wantKind || ev.Reason != tc.wantReason {
				t.Fatalf("expected kind %d reason %q, got %+v", tc.wantKind, tc.wantReason, ev)
			}
			if tc.wantKind != webhookIgnored && ev.ProviderReference != "2S-x-1" {
				t.Fatalf("expected reference 2S-x-1, got %q", ev.ProviderReference)
			}
		})
	}
}

func TestHyparrowParseWebhook_RejectsBadSignatures(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := newHyparrowProvider(testHyparrowConfig("https://example.test"))
	p.now = func() time.Time { return now }
	body := []byte(`{"event":"transfer.settled","data":{"clientReference":"2S-x-1"}}`)

	tampered := signedHyparrowHeader("whsec", now, body)
	stale := signedHyparrowHeader("whsec", now.Add(-10*time.Minute), body)
	wrongSecret := signedHyparrowHeader("other", now, body)
	missing := http.Header{}

	for name, h := range map[string]http.Header{"wrong secret": wrongSecret, "stale timestamp (replay)": stale, "missing headers": missing} {
		if _, err := p.parseWebhook(h, body); !errors.Is(err, ErrInvalidWebhookSignature) {
			t.Fatalf("%s: expected ErrInvalidWebhookSignature, got %v", name, err)
		}
	}
	if _, err := p.parseWebhook(tampered, []byte(strings.Replace(string(body), "settled", "reversed", 1))); !errors.Is(err, ErrInvalidWebhookSignature) {
		t.Fatalf("tampered body: expected ErrInvalidWebhookSignature, got %v", err)
	}

	noSecret := testHyparrowConfig("https://example.test")
	noSecret.WebhookSecret = ""
	if _, err := newHyparrowProvider(noSecret).parseWebhook(tampered, body); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("no webhook secret: expected ErrUnknownProvider, got %v", err)
	}
}
