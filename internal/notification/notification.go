// Package notification is Phase 7: the terminal consumer of the domain
// events every other module publishes (ARCHITECTURE.md's module map —
// "Webhook subscriber config, delivery log, dead-letter queue"). It
// delivers a signed webhook to the owning tenant for every session/
// settlement event, and an internal ops email for the handful of events
// that need a human paged (settlement.failed/reversed, compliance's
// hold_created).
//
// Every event this package subscribes to carries tenant_id directly in its
// payload (backfilled as part of this phase — see internal/session/events.go
// and internal/settlement/webhook.go) precisely so this package never needs
// to depend on session.Store or settlement.Store to resolve a delivery
// destination. Its only real dependency is tenant.Store, via the narrow
// TenantWebhookLookup interface below — the same "own tables only, cross via
// an exported interface" discipline session.EntitlementChecker/
// settlement.FeeResolver already establish.
//
// Same "eventbus handler claims a local row, a ticker-driven worker does the
// real network call" split settlement uses, for the same reason:
// eventbus.Handler must be a fast, local-DB-only write — an HTTP POST or SMTP
// call must never run inside its transaction.
package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

// Channel is which delivery mechanism a notification_deliveries row uses.
type Channel string

const (
	ChannelWebhook Channel = "webhook"
	ChannelEmail   Channel = "email"
)

// Status is a delivery row's lifecycle state.
type Status string

const (
	StatusPendingDispatch Status = "pending_dispatch"
	StatusDelivered       Status = "delivered"
	StatusDeadLetter      Status = "dead_letter"
)

// backoffSteps is the retry schedule before a delivery reaches dead_letter —
// a compiled-in design decision, not an ops knob, same convention as
// settlement.RetryBackoff/MaxAutoRetryAttempts. 5 attempts total.
var backoffSteps = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
	1 * time.Hour,
	6 * time.Hour,
}

var ErrNotFound = errors.New("notification: not found")

// Delivery is one outbound notification attempt record. TenantID is nullable
// — ledger.drift_detected (Phase 8) can be about a platform/omnibus account
// with no owning tenant at all, unlike every other event this package
// routes, which are always tenant-scoped. Only ChannelEmail deliveries can
// have a nil TenantID; sendWebhook (webhook.go) always has a real one, since
// webhookEvents only ever contains tenant-scoped event types.
type Delivery struct {
	ID            uuid.UUID
	TenantID      *uuid.UUID
	EventType     string
	AggregateType string
	AggregateID   uuid.UUID
	Channel       Channel
	Destination   string
	Payload       json.RawMessage
	Status        Status
	AttemptCount  int
	NextAttemptAt time.Time
	LastError     *string
	DeliveredAt   *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// TenantWebhookLookup is the one tenant-module capability this package
// needs — narrow, not internal/tenant.Store directly, same convention
// session.EntitlementChecker/settlement.FeeResolver already establish for
// keeping the tenant module boundary hard everywhere else touches it.
type TenantWebhookLookup interface {
	WebhookConfig(ctx context.Context, tenantID uuid.UUID) (webhookURL, signingSecret string, ok bool, err error)
}

// EmailProviderConfig configures the internal ops-alert email adapter.
// Structurally identical to config.EmailProviderConfig (cmd/server converts
// between them the same way it does for settlement's provider configs).
// Disabled by default — Provider selects which vendor adapter New builds
// (see providers.go's newEmailProvider); an empty/unrecognized value falls
// back to a stub that reports itself disabled, so a misconfigured
// deployment fails safe (deliveries dead-letter immediately, ops can see
// why) rather than panicking at startup.
type EmailProviderConfig struct {
	Enabled bool
	// Provider selects the vendor adapter — "resend" today, more can be
	// added to providers.go's newEmailProvider without touching anything
	// else in this package.
	Provider    string
	APIURL      string // optional override; each adapter has its own real default
	APIKey      string
	FromAddress string
}

// Config is what main.go builds from *config.Config to construct a Store —
// same convention treasury.Config/settlement.Config follow.
type Config struct {
	OpsAlertEmail string
	Email         EmailProviderConfig
}

// Store is the notification module's single entry point.
type Store struct {
	pool          *db.Pool
	tenantStore   TenantWebhookLookup
	cfg           Config
	emailProvider EmailProvider
	httpClient    *http.Client

	// bus is nil-safe — a Store built without one just never has
	// RegisterEventHandlers wired up, same convention every other module's
	// bus field already establishes.
	bus *eventbus.Bus
}

func New(pool *db.Pool, tenantStore TenantWebhookLookup, cfg Config) *Store {
	return &Store{
		pool:          pool,
		tenantStore:   tenantStore,
		cfg:           cfg,
		emailProvider: newEmailProvider(cfg.Email),
		httpClient:    &http.Client{Timeout: 8 * time.Second},
	}
}

// SetEventBus wires this Store to subscribe to domain events. Optional —
// nil-safe, see the bus field's doc comment.
func (s *Store) SetEventBus(bus *eventbus.Bus) {
	s.bus = bus
}

const deliveryColumns = `id, tenant_id, event_type, aggregate_type, aggregate_id,
	channel, destination, payload, status, attempt_count, next_attempt_at,
	last_error, delivered_at, created_at, updated_at`

func scanDelivery(row pgx.Row) (*Delivery, error) {
	var d Delivery
	var channel, status string
	err := row.Scan(
		&d.ID, &d.TenantID, &d.EventType, &d.AggregateType, &d.AggregateID,
		&channel, &d.Destination, &d.Payload, &status, &d.AttemptCount, &d.NextAttemptAt,
		&d.LastError, &d.DeliveredAt, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	d.Channel = Channel(channel)
	d.Status = Status(status)
	return &d, nil
}

func (s *Store) GetDelivery(ctx context.Context, id uuid.UUID) (*Delivery, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+deliveryColumns+` FROM notification_deliveries WHERE id = $1`, id)
	d, err := scanDelivery(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("notification: get delivery: %w", err)
	}
	return d, nil
}
