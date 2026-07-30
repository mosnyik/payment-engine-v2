// Phase 8's reconciliation job: ledger_balances (000004_create_ledger.up.sql)
// is a derived cache, "always reconstructable by summing ledger_entries...
// never the source of truth". This file is the periodic check that catches
// the cache actually disagreeing with a fresh sum — flags it for ops via
// ledger_discrepancies + a ledger.drift_detected notification, never
// auto-repairs it. sweepOnce can never observe drift produced by Post
// itself (that's the same transaction, atomically), only external causes:
// manual DB intervention, a bug elsewhere writing to these tables directly,
// or corruption.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

var ErrDiscrepancyAlreadyResolved = errors.New("ledger: discrepancy not found or already resolved")

// Discrepancy is one detected drift occurrence between a ledger account's
// cached ledger_balances row and a fresh sum of its ledger_entries. One row
// per occurrence, not a mutable "current state" row — see
// 000023_create_ledger_discrepancies.up.sql.
type Discrepancy struct {
	ID              uuid.UUID
	AccountID       uuid.UUID
	CachedBalance   decimal.Decimal
	ComputedBalance decimal.Decimal
	DriftAmount     decimal.Decimal
	DetectedAt      time.Time
	ResolvedAt      *time.Time
	ResolvedBy      *uuid.UUID
	ResolutionNote  *string
}

const discrepancyColumns = `id, account_id, cached_balance, computed_balance, drift_amount,
	detected_at, resolved_at, resolved_by, resolution_note`

func scanDiscrepancy(row pgx.Row) (*Discrepancy, error) {
	var d Discrepancy
	err := row.Scan(
		&d.ID, &d.AccountID, &d.CachedBalance, &d.ComputedBalance, &d.DriftAmount,
		&d.DetectedAt, &d.ResolvedAt, &d.ResolvedBy, &d.ResolutionNote,
	)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ReconcileJob is ticker-driven — same shape as session.TTLJob/rate.FetchJob:
// started with `go job.Run(ctx)` from main.go.
type ReconcileJob struct {
	ledger       *Ledger
	pollInterval time.Duration
}

func NewReconcileJob(ledger *Ledger, pollInterval time.Duration) *ReconcileJob {
	return &ReconcileJob{ledger: ledger, pollInterval: pollInterval}
}

// Run blocks until ctx is cancelled, sweeping every pollInterval.
func (j *ReconcileJob) Run(ctx context.Context) {
	ticker := time.NewTicker(j.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.ledger.sweepOnce(ctx); err != nil {
				log.Printf("ledger: reconcile sweep: %v", err)
			}
		}
	}
}

// sweepOnce compares every account's cached ledger_balances row against a
// fresh SUM(ledger_entries) computed in the same query — one round trip
// covering every account, rather than fetching accounts and summing each
// individually. An account with no entries at all never has a
// ledger_balances row (GetBalance's own doc comment) and so never appears
// here, which is correct: it can't have drifted.
func (l *Ledger) sweepOnce(ctx context.Context) error {
	rows, err := l.pool.Query(ctx,
		`SELECT lb.account_id, la.tenant_id, lb.balance, COALESCE(fresh.computed, 0)
		 FROM ledger_balances lb
		 JOIN ledger_accounts la ON la.id = lb.account_id
		 LEFT JOIN (
		     SELECT account_id,
		            SUM(CASE WHEN direction = 'debit' THEN amount ELSE -amount END) AS computed
		     FROM ledger_entries
		     GROUP BY account_id
		 ) fresh ON fresh.account_id = lb.account_id
		 WHERE lb.balance != COALESCE(fresh.computed, 0)`,
	)
	if err != nil {
		return fmt.Errorf("ledger: reconcile sweep: %w", err)
	}
	type drift struct {
		accountID uuid.UUID
		tenantID  *uuid.UUID
		cached    decimal.Decimal
		computed  decimal.Decimal
	}
	var drifted []drift
	for rows.Next() {
		var d drift
		if err := rows.Scan(&d.accountID, &d.tenantID, &d.cached, &d.computed); err != nil {
			rows.Close()
			return fmt.Errorf("ledger: reconcile sweep: scan: %w", err)
		}
		drifted = append(drifted, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger: reconcile sweep: %w", err)
	}

	for _, d := range drifted {
		if err := l.flagDrift(ctx, d.accountID, d.tenantID, d.cached, d.computed); err != nil {
			return err
		}
	}
	return nil
}

// flagDrift opens one ledger_discrepancies row and publishes
// ledger.drift_detected in the same transaction, unless this account already
// has an unresolved discrepancy open — otherwise every poll interval would
// re-flag and re-page ops about the same still-broken account forever.
func (l *Ledger) flagDrift(ctx context.Context, accountID uuid.UUID, tenantID *uuid.UUID, cached, computed decimal.Decimal) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ledger: flag drift: begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	var alreadyOpen bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM ledger_discrepancies WHERE account_id = $1 AND resolved_at IS NULL)`,
		accountID,
	).Scan(&alreadyOpen); err != nil {
		return fmt.Errorf("ledger: flag drift: check open: %w", err)
	}
	if alreadyOpen {
		return nil
	}

	driftAmount := cached.Sub(computed)
	var discrepancyID uuid.UUID
	if err := tx.QueryRow(ctx,
		`INSERT INTO ledger_discrepancies (account_id, cached_balance, computed_balance, drift_amount)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		accountID, cached, computed, driftAmount,
	).Scan(&discrepancyID); err != nil {
		return fmt.Errorf("ledger: flag drift: insert: %w", err)
	}

	if l.bus != nil {
		// tenantID is nil for a platform/omnibus account — notification's
		// ChannelEmail path (the only channel ledger.drift_detected routes
		// to) doesn't need one, and notification_deliveries.tenant_id is
		// nullable for exactly this event (000022_notification_deliveries_
		// nullable_tenant.up.sql).
		payload, err := json.Marshal(map[string]any{
			"tenant_id":    tenantID,
			"account_id":   accountID.String(),
			"drift_amount": driftAmount.String(),
		})
		if err != nil {
			return fmt.Errorf("ledger: marshal ledger.drift_detected payload: %w", err)
		}
		if err := l.bus.Publish(ctx, tx, eventbus.Event{
			EventType:     "ledger.drift_detected",
			AggregateType: "ledger_account",
			AggregateID:   accountID,
			Payload:       payload,
		}); err != nil {
			return fmt.Errorf("ledger: publish ledger.drift_detected: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ledger: flag drift: commit: %w", err)
	}
	return nil
}

// ListDiscrepancies returns discrepancies matching resolved, newest first —
// the ops queue surface (cmd/server/ledger_handlers.go).
func (l *Ledger) ListDiscrepancies(ctx context.Context, resolved bool) ([]Discrepancy, error) {
	clause := "resolved_at IS NULL"
	if resolved {
		clause = "resolved_at IS NOT NULL"
	}
	rows, err := l.pool.Query(ctx,
		`SELECT `+discrepancyColumns+` FROM ledger_discrepancies WHERE `+clause+` ORDER BY detected_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("ledger: list discrepancies: %w", err)
	}
	defer rows.Close()

	var discrepancies []Discrepancy
	for rows.Next() {
		d, err := scanDiscrepancy(rows)
		if err != nil {
			return nil, fmt.Errorf("ledger: list discrepancies: scan: %w", err)
		}
		discrepancies = append(discrepancies, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: list discrepancies: %w", err)
	}
	return discrepancies, nil
}

// ResolveDiscrepancy is the manual review action: ops has investigated a
// flagged drift and closes it. Compare-and-set on resolved_at IS NULL, same
// discipline every other status transition in this codebase uses. Never
// rewrites ledger_balances — see this file's package doc comment.
func (l *Ledger) ResolveDiscrepancy(ctx context.Context, id, adminID uuid.UUID, note string) (*Discrepancy, error) {
	row := l.pool.QueryRow(ctx,
		`UPDATE ledger_discrepancies SET resolved_at = now(), resolved_by = $2, resolution_note = $3
		 WHERE id = $1 AND resolved_at IS NULL
		 RETURNING `+discrepancyColumns,
		id, adminID, note,
	)
	d, err := scanDiscrepancy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDiscrepancyAlreadyResolved
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: resolve discrepancy: %w", err)
	}
	return d, nil
}
