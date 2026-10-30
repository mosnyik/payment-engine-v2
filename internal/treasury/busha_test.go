package treasury

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestBushaBusinessReserveDeposit(t *testing.T) {
	for _, scenario := range []string{"success", "unauthorized", "wrong-network", "wrong-asset", "wrong-amount", "expired", "missing-id", "empty-response"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			expires := time.Now().Add(time.Hour).UTC()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-api-key" {
					t.Error("incorrect Busha authentication or method")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				if scenario == "unauthorized" {
					w.WriteHeader(401)
					return
				}
				if calls == 1 {
					if r.URL.Path != "/v1/quotes" || body["source_currency"] != "USDT" || body["target_currency"] != "USDT" || body["source_amount"] != "12.345678" || body["pay_out"] != nil {
						t.Errorf("wrong deposit quote: %v", body)
					}
					payIn, ok := body["pay_in"].(map[string]any)
					if !ok || payIn["type"] != "address" || payIn["network"] != "TRX" {
						t.Errorf("wrong pay-in: %v", body)
					}
					w.WriteHeader(201)
					_, _ = w.Write([]byte(`{"status":"success","data":{"id":"QUO_test"}}`))
					return
				}
				if calls != 2 || r.URL.Path != "/v1/transfers" || body["quote_id"] != "QUO_test" {
					t.Errorf("wrong transfer request: %v", body)
				}
				if scenario == "empty-response" {
					_, _ = w.Write([]byte(`{"status":"success","data":null}`))
					return
				}
				transfer := bushaTransfer{ID: "TRF_test", SourceCurrency: "USDT", TargetCurrency: "USDT", SourceAmount: decimal.RequireFromString("12.345678"), Status: "pending"}
				transfer.PayIn.Type, transfer.PayIn.Network, transfer.PayIn.Address = "address", "TRX", "test-deposit-address"
				transfer.PayIn.Memo, transfer.PayIn.ExpiresAt = "memo", &expires
				switch scenario {
				case "wrong-network":
					transfer.PayIn.Network = "ETH"
				case "wrong-asset":
					transfer.TargetCurrency = "NGN"
				case "wrong-amount":
					transfer.SourceAmount = decimal.NewFromInt(5)
				case "expired":
					past := time.Now().Add(-time.Hour)
					transfer.PayIn.ExpiresAt = &past
				case "missing-id":
					transfer.ID = ""
				}
				w.WriteHeader(201)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": transfer})
			}))
			defer srv.Close()
			p := newBushaProvider(CollectionProviderConfig{Enabled: true, APIURL: srv.URL + "/", APIKey: "test-api-key", WebhookSecret: "secret"})
			got, err := p.ReserveDeposit(context.Background(), uuid.New(), "USDT", "TRX", DepositRequest{Amount: decimal.RequireFromString("12.345678")})
			if scenario != "success" {
				if err == nil {
					t.Fatal("expected malformed/rejected response to fail")
				}
				if scenario == "unauthorized" && calls != 1 {
					t.Fatal("must not finalize a failed quote")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if calls != 2 || got.Address != "test-deposit-address" || got.ProviderReference != "TRF_test" || got.Amount != "12.345678" || got.AddressTag != "memo" || !got.ExpiresAt.Equal(expires) {
				t.Fatalf("unexpected instructions: %+v", got)
			}
		})
	}
}

func TestBushaBusinessSignatureAndDisabledProvider(t *testing.T) {
	body := []byte(`{"event":"transfer.pending"}`)
	mac := hmac.New(sha256.New, []byte("shared-secret"))
	mac.Write(body)
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if err := VerifyBushaSignature("shared-secret", body, sig); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		secret string
		body   []byte
		sig    string
	}{
		{"", body, ComputeBushaSignature("", body)},
		{"shared-secret", body, ""},
		{"shared-secret", []byte(`{"event":"transfer.completed"}`), sig},
		{"shared-secret", body, ComputeWebhookSignature("shared-secret", body)},
	} {
		if !errors.Is(VerifyBushaSignature(tc.secret, tc.body, tc.sig), ErrInvalidWebhookSignature) {
			t.Fatal("accepted invalid signature")
		}
	}
	s := New(nil, nil, Config{})
	if !errors.Is(s.HandleDepositWebhook(context.Background(), body, sig), ErrInvalidWebhookSignature) {
		t.Fatal("disabled Busha accepted a webhook")
	}
}

func TestBushaBusinessEventClassification(t *testing.T) {
	for _, tc := range []struct {
		event, status, amount, want string
		relevant, invalid           bool
	}{
		{"transfer.pending", "pending", "10", "", false, false},
		{"transfer.processing", "processing", "10", "detected", true, false},
		{"transfer.funds_received", "funds_received", "10", "confirmed", true, false},
		{"transfer.completed", "completed", "10", "confirmed", true, false},
		{"transfer.failed", "failed", "10", "", false, false},
		{"transfer.completed", "pending", "10", "", false, true},
		{"transfer.completed", "completed", "-1", "", false, true},
		{"transfer.completed", "completed", "0", "", false, true},
		{"transfer.completed", "completed", "0.000000001", "", false, true},
	} {
		p := bushaWebhookPayload{EventType: DepositEventType(tc.event), Data: bushaTransfer{ID: "TRF_one", Status: tc.status, SourceCurrency: "USDT", TargetCurrency: "USDT", SourceAmount: decimal.RequireFromString(tc.amount)}}
		status, relevant, err := classifyBushaDeposit(p)
		if status != tc.want || relevant != tc.relevant || (err != nil) != tc.invalid {
			t.Fatalf("%+v => %s %v %v", tc, status, relevant, err)
		}
	}
}

func TestBushaBusinessRequiresAmountAndCredentials(t *testing.T) {
	p := newBushaProvider(CollectionProviderConfig{Enabled: true})
	if _, err := p.ReserveDeposit(context.Background(), uuid.New(), "USDT", "TRX", DepositRequest{}); err == nil {
		t.Fatal("accepted unconfigured deposit")
	}
	_, err := p.ReserveAddress(context.Background(), uuid.New(), "USDT", "TRX")
	if err == nil || !strings.Contains(err.Error(), "amount") {
		t.Fatal("address-only call must fail")
	}
}
