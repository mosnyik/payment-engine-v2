package treasury

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/corridor"
)

type fakeTenantWebhookLookup struct {
	url    string
	secret string
	ok     bool
}

func (f *fakeTenantWebhookLookup) GetWebhookConfig(ctx context.Context, tenantID uuid.UUID) (string, string, bool, error) {
	return f.url, f.secret, f.ok, nil
}

// TestDeliverSignedWebhook_SignsAndDelivers exercises the actual delivery
// mechanics (signing + POST + retry) against a local httptest.Server —
// notifyTenant itself can't be fully exercised end-to-end this way, since
// its URL re-validation correctly rejects loopback addresses (including a
// local test server's), by design (see TestNotifyTenant_RejectsLoopback).
func TestDeliverSignedWebhook_SignsAndDelivers(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})

	var receivedBody []byte
	var receivedSignature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		receivedSignature = r.Header.Get("X-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	s.tenantWebhookClient = server.Client()
	s.tenantWebhookMaxRetries = 1

	const secret = "test-tenant-webhook-secret"
	payload := tenantNotificationPayload{
		Event:       "deposit.confirmed",
		TenantID:    uuid.New(),
		CryptoAsset: "USDT",
		Address:     "0xabc",
		Amount:      "100",
		TxReference: "0xtx",
	}

	if err := s.deliverSignedWebhook(context.Background(), server.URL, secret, payload); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if receivedSignature == "" {
		t.Fatalf("expected a signature header to be sent")
	}
	if err := VerifyWebhookSignature(secret, receivedBody, receivedSignature); err != nil {
		t.Fatalf("received signature did not verify against received body: %v", err)
	}

	var decoded tenantNotificationPayload
	if err := json.Unmarshal(receivedBody, &decoded); err != nil {
		t.Fatalf("decode received body: %v", err)
	}
	if decoded.Event != payload.Event || decoded.TxReference != payload.TxReference {
		t.Fatalf("received payload doesn't match what was sent: %+v", decoded)
	}
}

func TestDeliverSignedWebhook_RetriesOnFailureThenSucceeds(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})

	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	s.tenantWebhookClient = server.Client()
	s.tenantWebhookMaxRetries = 3

	err := s.deliverSignedWebhook(context.Background(), server.URL, "secret", tenantNotificationPayload{Event: "deposit.detected"})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", attempts)
	}
}

func TestNotifyTenant_RejectsLoopback(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	tenantID := createTestTenant(t, pool)

	// Any local address (this is what a real loopback SSRF attempt would
	// look like) must be rejected at delivery time, not just registration
	// time — this is the exact DNS-rebinding gap tenant.SetWebhookURL's
	// doc comment flags, closed here.
	s.SetTenantWebhookLookup(&fakeTenantWebhookLookup{url: "https://127.0.0.1/hook", secret: "s", ok: true})

	r := AddressReservation{ID: uuid.New(), TenantID: tenantID, CryptoAsset: "ETH", CryptoNetwork: "ethereum", Address: "0xabc"}
	tx := ChainTransaction{TxID: "0xtest", Amount: decimal.NewFromInt(1)}

	err := s.notifyTenant(context.Background(), tenantID, "deposit.confirmed", r, tx)
	if err == nil {
		t.Fatalf("expected notifyTenant to reject a loopback webhook url")
	}
}

func TestNotifyTenant_NoWebhookRegisteredIsNoOp(t *testing.T) {
	pool := openTestPool(t)
	s := New(pool, corridor.New(pool), Config{})
	s.SetTenantWebhookLookup(&fakeTenantWebhookLookup{ok: false})

	tenantID := createTestTenant(t, pool)
	r := AddressReservation{ID: uuid.New(), TenantID: tenantID, CryptoAsset: "ETH", CryptoNetwork: "ethereum", Address: "0xabc"}
	tx := ChainTransaction{TxID: "0xtest", Amount: decimal.NewFromInt(1)}

	if err := s.notifyTenant(context.Background(), tenantID, "deposit.confirmed", r, tx); err != nil {
		t.Fatalf("expected no-op when no webhook is registered, got %v", err)
	}
}
