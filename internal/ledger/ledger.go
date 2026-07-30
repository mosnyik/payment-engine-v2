// Package ledger is the double-entry source of truth for every value
// movement in the system. Post is the only write path — nothing else is
// permitted to write to ledger_entries or ledger_balances directly, and
// every other module posts to this rather than being its own source of
// truth for "has this happened".
//
// Sign convention: an account's balance is SUM(debits) - SUM(credits),
// uniformly for every account regardless of its accounting nature (asset,
// liability, revenue...). This is a raw net-debit balance, not an
// accounting-presentation value — a liability account will normally show a
// negative balance under this convention, which is correct and expected,
// not a bug. Whether to flip the sign for human-facing presentation is a
// reporting-layer concern, not this package's.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
	"github.com/sirfi/payment-engine-v2/internal/platform/eventbus"
)

type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

var (
	// ErrAlreadyPosted means a transaction with this idempotency key was
	// already posted. This is the atomic claim mechanism callers rely on —
	// e.g. settlement dispatch posts before calling the external provider;
	// if the post is rejected as a duplicate, another trigger already
	// claimed this payout and the caller must abort rather than dispatch
	// again. It is an expected, named outcome, not a generic failure.
	ErrAlreadyPosted = errors.New("ledger: transaction with this idempotency key already posted")
	ErrNoEntries     = errors.New("ledger: transaction must have at least one entry")
	ErrUnbalanced    = errors.New("ledger: entries do not balance per asset")
	ErrInvalidEntry  = errors.New("ledger: invalid entry")
)

type Entry struct {
	AccountID uuid.UUID
	Direction Direction
	Amount    decimal.Decimal
	AssetCode string
}

type Transaction struct {
	// IdempotencyKey is the atomic claim: unique per real-world event, e.g.
	// "settlement_payout:session_123" or "settlement_payout:session_123:attempt_2"
	// for a retry. Never derive it from anything that changes on retry of
	// the same logical attempt (that would defeat the claim).
	IdempotencyKey string
	TxnType        string // "deposit_confirmed", "fx_conversion", "sweep", "settlement_payout", "fee", "reversal", "manual_adjustment", ...
	ReferenceType  string // "session", "settlement", ...
	ReferenceID    uuid.UUID
	CreatedBy      string // owning module name
	Entries        []Entry
}

type Ledger struct {
	pool *db.Pool

	// bus is nil-safe, same convention every other module's bus field
	// already establishes — used only by Phase 8's reconciliation job
	// (reconcile.go) to publish ledger.drift_detected. Post itself never
	// needed to publish anything.
	bus *eventbus.Bus
}

func New(pool *db.Pool) *Ledger {
	return &Ledger{pool: pool}
}

// SetEventBus wires this Ledger to publish domain events. Optional —
// nil-safe, see the bus field's doc comment.
func (l *Ledger) SetEventBus(bus *eventbus.Bus) {
	l.bus = bus
}

// Post is the only write path into the ledger. It validates that entries
// balance per asset_code, atomically claims the idempotency key, inserts
// the transaction and its entries, and updates each touched account's
// cached balance — all in one DB transaction, so a partial post can never
// be observed.
func (l *Ledger) Post(ctx context.Context, txn Transaction) (uuid.UUID, error) {
	if len(txn.Entries) == 0 {
		return uuid.Nil, ErrNoEntries
	}
	for i, e := range txn.Entries {
		if e.Direction != Debit && e.Direction != Credit {
			return uuid.Nil, fmt.Errorf("%w: entry %d: direction must be debit or credit, got %q", ErrInvalidEntry, i, e.Direction)
		}
		if e.Amount.Sign() <= 0 {
			return uuid.Nil, fmt.Errorf("%w: entry %d: amount must be positive, got %s", ErrInvalidEntry, i, e.Amount)
		}
		if e.AssetCode == "" {
			return uuid.Nil, fmt.Errorf("%w: entry %d: asset_code is required", ErrInvalidEntry, i)
		}
	}
	if err := validateBalance(txn.Entries); err != nil {
		return uuid.Nil, err
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ledger: begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	var txnID uuid.UUID
	err = tx.QueryRow(ctx,
		`INSERT INTO ledger_transactions (idempotency_key, txn_type, reference_type, reference_id, created_by)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (idempotency_key) DO NOTHING
		 RETURNING id`,
		txn.IdempotencyKey, txn.TxnType, txn.ReferenceType, txn.ReferenceID, txn.CreatedBy,
	).Scan(&txnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrAlreadyPosted
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("ledger: insert transaction: %w", err)
	}

	for _, e := range txn.Entries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (transaction_id, account_id, direction, amount, asset_code)
			 VALUES ($1, $2, $3, $4, $5)`,
			txnID, e.AccountID, string(e.Direction), e.Amount, e.AssetCode,
		); err != nil {
			return uuid.Nil, fmt.Errorf("ledger: insert entry: %w", err)
		}

		delta := e.Amount
		if e.Direction == Credit {
			delta = e.Amount.Neg()
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_balances (account_id, balance, updated_at)
			 VALUES ($1, $2, now())
			 ON CONFLICT (account_id) DO UPDATE
			 SET balance = ledger_balances.balance + $2, updated_at = now()`,
			e.AccountID, delta,
		); err != nil {
			return uuid.Nil, fmt.Errorf("ledger: update balance: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: commit: %w", err)
	}

	return txnID, nil
}

// validateBalance enforces the core invariant: within a transaction,
// entries balance independently per asset_code. A currency conversion is
// two separately-balanced transactions bridged by a clearing account, never
// one mixed-unit transaction — see ARCHITECTURE.md §6.
func validateBalance(entries []Entry) error {
	sums := make(map[string]decimal.Decimal)
	for _, e := range entries {
		sum := sums[e.AssetCode]
		if e.Direction == Debit {
			sums[e.AssetCode] = sum.Add(e.Amount)
		} else {
			sums[e.AssetCode] = sum.Sub(e.Amount)
		}
	}
	for asset, sum := range sums {
		if !sum.IsZero() {
			return fmt.Errorf("%w: asset %s debits and credits differ by %s", ErrUnbalanced, asset, sum)
		}
	}
	return nil
}

// GetOrCreateAccount returns the account matching (tenantID, accountType,
// assetCode), creating it if it doesn't exist yet. tenantID is nil for
// platform/omnibus accounts (treasury, clearing, fee revenue, ...).
func (l *Ledger) GetOrCreateAccount(ctx context.Context, tenantID *uuid.UUID, accountType, assetCode, unitType, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := l.pool.QueryRow(ctx,
		`INSERT INTO ledger_accounts (tenant_id, account_type, asset_code, unit_type, name)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (tenant_id, account_type, asset_code) DO UPDATE
		 SET name = ledger_accounts.name
		 RETURNING id`,
		tenantID, accountType, assetCode, unitType, name,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ledger: get or create account: %w", err)
	}
	return id, nil
}

// GetBalance returns an account's current cached balance. An account with
// no entries posted yet has a zero balance, not an error.
func (l *Ledger) GetBalance(ctx context.Context, accountID uuid.UUID) (decimal.Decimal, error) {
	var balance decimal.Decimal
	err := l.pool.QueryRow(ctx, `SELECT balance FROM ledger_balances WHERE account_id = $1`, accountID).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, nil
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("ledger: get balance: %w", err)
	}
	return balance, nil
}
