package notification

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListDeliveries is the ops dead-letter-queue surface (and general delivery
// visibility) — same shape as settlement.ListSettlements.
func (s *Store) ListDeliveries(ctx context.Context, status Status) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+deliveryColumns+` FROM notification_deliveries WHERE status = $1 ORDER BY created_at DESC`,
		string(status),
	)
	if err != nil {
		return nil, fmt.Errorf("notification: list deliveries: %w", err)
	}
	defer rows.Close()

	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("notification: scan delivery: %w", err)
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notification: list deliveries: %w", err)
	}
	return out, nil
}

// ListDeliveriesByTenant returns a tenant's own notification deliveries,
// newest first — the portal dashboard's read surface. Optionally filtered
// by status; nil means all statuses. Delivery.TenantID is *uuid.UUID
// (migration 000022 — ledger.drift_detected isn't tenant-scoped), but this
// filter is always a plain equality against a real, non-null tenant_id.
func (s *Store) ListDeliveriesByTenant(ctx context.Context, tenantID uuid.UUID, status *Status) ([]Delivery, error) {
	var statusFilter *string
	if status != nil {
		v := string(*status)
		statusFilter = &v
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+deliveryColumns+` FROM notification_deliveries
		 WHERE tenant_id = $1 AND ($2::text IS NULL OR status = $2)
		 ORDER BY created_at DESC`,
		tenantID, statusFilter,
	)
	if err != nil {
		return nil, fmt.Errorf("notification: list deliveries by tenant: %w", err)
	}
	defer rows.Close()

	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("notification: list deliveries by tenant: scan: %w", err)
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notification: list deliveries by tenant: %w", err)
	}
	return out, nil
}

// RetryDelivery is the ops-triggered force-retry path for a dead-lettered
// delivery — resets attempt_count so it gets the full backoff schedule
// again, same "human intervenes, gets a fresh shot" shape
// settlement.RetryPayout already establishes for settlement_failed.
func (s *Store) RetryDelivery(ctx context.Context, id uuid.UUID) (*Delivery, error) {
	row := s.pool.QueryRow(ctx,
		`UPDATE notification_deliveries
		 SET status = 'pending_dispatch', attempt_count = 0, next_attempt_at = now(), last_error = NULL, updated_at = now()
		 WHERE id = $1 AND status = 'dead_letter'
		 RETURNING `+deliveryColumns,
		id,
	)
	d, err := scanDelivery(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("notification: retry delivery: %w", err)
	}
	return d, nil
}
