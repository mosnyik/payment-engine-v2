package treasury

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/platform/webhookurl"
)

// TenantWebhookLookup resolves a tenant's registered webhook URL and
// signing secret — treasury depends on this interface, not on
// internal/tenant directly, so the module boundary stays a hard one (same
// convention gateway.CredentialLookup already established for
// tenant/gateway). internal/tenant.Store.WebhookConfig implements it.
type TenantWebhookLookup interface {
	GetWebhookConfig(ctx context.Context, tenantID uuid.UUID) (url, signingSecret string, ok bool, err error)
}

// SetTenantWebhookLookup wires tenant-custom-wallet deposit notifications.
// Optional — nil-safe: a Store with none set just skips notifications
// (logged, not fatal), which is what every existing test already does
// without needing to construct one.
func (s *Store) SetTenantWebhookLookup(lookup TenantWebhookLookup) {
	s.tenantWebhooks = lookup
}

// tenantNotificationPayload is what a tenant's webhook endpoint receives
// for a tenant-provided-wallet deposit event.
type tenantNotificationPayload struct {
	Event         string    `json:"event"` // "deposit.detected" | "deposit.confirmed"
	TenantID      uuid.UUID `json:"tenant_id"`
	ReservationID uuid.UUID `json:"reservation_id"`
	CryptoAsset   string    `json:"crypto_asset"`
	CryptoNetwork string    `json:"crypto_network"`
	Address       string    `json:"address"`
	Amount        string    `json:"amount"`
	TxReference   string    `json:"tx_reference"`
	Timestamp     time.Time `json:"timestamp"`
}

// notifyTenant signs and delivers one deposit-event webhook for a
// tenant-provided-wallet reservation. Intentionally minimal compared to
// what Phase 7 (notification) will eventually provide: bounded retries,
// no persistent delivery log, no dead-letter queue — this exists because
// the tenant-custom-wallet feature needs webhook confirmation to actually
// work now, not because it replaces Phase 7's eventual scope.
func (s *Store) notifyTenant(ctx context.Context, tenantID uuid.UUID, event string, r AddressReservation, tx ChainTransaction) error {
	url, signingSecret, ok, err := s.tenantWebhooks.GetWebhookConfig(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("treasury: get tenant webhook config: %w", err)
	}
	if !ok {
		return nil // no webhook registered — nothing to deliver
	}

	// Re-validate immediately before sending, not just at registration —
	// DNS can rebind between the two (the exact gap tenant.SetWebhookURL's
	// doc comment flags; this closes it for this sender).
	if err := webhookurl.Validate(url); err != nil {
		return fmt.Errorf("treasury: tenant webhook url failed re-validation: %w", err)
	}

	payload := tenantNotificationPayload{
		Event:         event,
		TenantID:      tenantID,
		ReservationID: r.ID,
		CryptoAsset:   r.CryptoAsset,
		CryptoNetwork: r.CryptoNetwork,
		Address:       r.Address,
		Amount:        tx.Amount.String(),
		TxReference:   tx.TxID,
		Timestamp:     time.Now(),
	}
	return s.deliverSignedWebhook(ctx, url, signingSecret, payload)
}

// deliverSignedWebhook signs payload and POSTs it to url with bounded
// retries — split out from notifyTenant so the delivery mechanics
// (signing, retry) are testable independent of URL re-validation (which
// rejects loopback addresses by design, including a local test server's).
func (s *Store) deliverSignedWebhook(ctx context.Context, url, signingSecret string, payload tenantNotificationPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("treasury: marshal tenant notification: %w", err)
	}
	signature := ComputeWebhookSignature(signingSecret, body)

	client := s.tenantWebhookClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	var lastErr error
	for attempt := 0; attempt < s.tenantWebhookMaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("treasury: build tenant webhook request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", signature)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("tenant webhook returned status %d", resp.StatusCode)
	}
	return fmt.Errorf("treasury: deliver tenant webhook after %d attempts: %w", s.tenantWebhookMaxRetries, lastErr)
}
