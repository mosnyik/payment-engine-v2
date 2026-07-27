package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

// webhookEvents/emailEvents are the compiled-in channel routing table
// (ARCHITECTURE.md's module map — "webhook delivery... + email, subscribing
// to session/settlement events"), not ops-configurable, same convention
// session.SessionTTL/settlement.MaxAutoRetryAttempts already establish for
// policy decisions. settlement.failed/reversed fan out to both channels —
// the tenant needs to know their payout failed, and so does ops
// (ARCHITECTURE.md §8: these are the two states that page ops).
// compliance.hold_created is ops-only: the tenant already gets a generic
// "under_review" status, never the specific hold details (ARCHITECTURE.md
// §5/§8).
var webhookEvents = map[string]bool{
	"session.created":           true,
	"session.deposit_detected":  true,
	"session.deposit_confirmed": true,
	"settlement.dispatched":     true,
	"settlement.completed":      true,
	"settlement.failed":         true,
	"settlement.reversed":       true,
}

var emailEvents = map[string]bool{
	"settlement.failed":       true,
	"settlement.reversed":     true,
	"compliance.hold_created": true,
}

// RegisterEventHandlers subscribes to every event this package routes
// somewhere. eventbus.Subscribe has no wildcard support, so each event type
// is listed explicitly — same convention session.RegisterEventHandlers
// already uses for its own two. Call once, before the bus's dispatcher
// starts. A no-op if this Store has no bus.
func (s *Store) RegisterEventHandlers() {
	if s.bus == nil {
		return
	}
	for eventType := range union(webhookEvents, emailEvents) {
		s.bus.Subscribe(eventType, s.handleEvent)
	}
}

func union(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// handleEvent inserts one notification_deliveries row per channel this
// event routes to. Per eventbus.Handler's doc comment: fast, local-DB-only
// writes using the supplied tx — the actual HTTP/SMTP send happens later,
// off DispatchWorker (dispatch.go). Resolving the tenant's webhook URL is a
// read via tenantStore's own pool (not tx) — safe since it needs no
// atomicity with the write below, same "a read elsewhere, the write in tx"
// shape settlement.handleDepositConfirmed already establishes.
func (s *Store) handleEvent(ctx context.Context, tx pgx.Tx, e eventbus.Event) error {
	tenantID, err := tenantIDFromPayload(e.Payload)
	if err != nil {
		return err
	}

	if webhookEvents[e.EventType] {
		if err := s.insertDelivery(ctx, tx, e, tenantID, ChannelWebhook); err != nil {
			return err
		}
	}
	if emailEvents[e.EventType] {
		if err := s.insertDelivery(ctx, tx, e, tenantID, ChannelEmail); err != nil {
			return err
		}
	}
	return nil
}

func tenantIDFromPayload(payload json.RawMessage) (uuid.UUID, error) {
	var p struct {
		TenantID uuid.UUID `json:"tenant_id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return uuid.Nil, fmt.Errorf("notification: parse tenant_id from event payload: %w", err)
	}
	return p.TenantID, nil
}

// insertDelivery resolves channel's destination and inserts one row, or
// silently does nothing when there's nowhere to send it — a tenant with no
// webhook configured, or no ops alert address configured, is not an error.
func (s *Store) insertDelivery(ctx context.Context, tx pgx.Tx, e eventbus.Event, tenantID uuid.UUID, channel Channel) error {
	var destination string
	switch channel {
	case ChannelWebhook:
		url, _, ok, err := s.tenantStore.WebhookConfig(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("notification: lookup webhook config: %w", err)
		}
		if !ok || url == "" {
			return nil
		}
		destination = url
	case ChannelEmail:
		if s.cfg.OpsAlertEmail == "" {
			return nil
		}
		destination = s.cfg.OpsAlertEmail
	}

	body, err := json.Marshal(map[string]any{
		"event_type":     e.EventType,
		"aggregate_type": e.AggregateType,
		"aggregate_id":   e.AggregateID,
		"tenant_id":      tenantID,
		"payload":        e.Payload,
		"created_at":     time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("notification: marshal delivery body: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO notification_deliveries (tenant_id, event_type, aggregate_type, aggregate_id, channel, destination, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (aggregate_id, event_type, channel) DO NOTHING`,
		tenantID, e.EventType, e.AggregateType, e.AggregateID, string(channel), destination, body,
	)
	if err != nil {
		return fmt.Errorf("notification: insert delivery: %w", err)
	}
	return nil
}
