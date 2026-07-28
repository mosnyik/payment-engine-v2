package notification

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
)

// webhookSignatureHeader is where a delivered webhook's HMAC signature is
// carried — same header name treasury/settlement use for their own inbound
// webhooks, kept consistent for tenants integrating both directions.
const webhookSignatureHeader = "X-Webhook-Signature"

// ComputeWebhookSignature computes the HMAC-SHA256 signature of body under
// secret — identical scheme to treasury.ComputeWebhookSignature/
// settlement.ComputeWebhookSignature, just used outbound here instead of
// inbound. This package defines its own copy rather than importing either;
// this codebase already tolerates that duplication (treasury and settlement
// each have their own independent copy for the same reason).
func ComputeWebhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// sendWebhook re-fetches the tenant's current signing secret at send time
// rather than snapshotting it on the delivery row — unlike destination
// (frozen at creation, so a later change to a tenant's URL can't redirect
// an in-flight retry), a rotated signing secret should apply to the very
// next attempt, not lag behind until a new event arrives.
func (s *Store) sendWebhook(ctx context.Context, d *Delivery) error {
	if d.TenantID == nil {
		return fmt.Errorf("notification: webhook delivery %s has no tenant_id", d.ID)
	}
	_, secret, ok, err := s.tenantStore.WebhookConfig(ctx, *d.TenantID)
	if err != nil {
		return fmt.Errorf("notification: lookup webhook config: %w", err)
	}
	if !ok || secret == "" {
		return fmt.Errorf("notification: tenant %s no longer has a webhook configured", *d.TenantID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Destination, bytes.NewReader(d.Payload))
	if err != nil {
		return fmt.Errorf("notification: build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhookSignatureHeader, ComputeWebhookSignature(secret, d.Payload))

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("notification: webhook post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notification: webhook returned status %d", resp.StatusCode)
	}
	return nil
}
