// Package db provides the shared Postgres connection pool used by every module.
package db

import (
	"context"
	"fmt"

	pgxdecimal "github.com/jackc/pgx-shopspring-decimal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgx connection pool. All modules take a *Pool, never open
// their own connections, so pooling/config lives in exactly one place.
type Pool struct {
	*pgxpool.Pool
}

// Open creates and validates a connection pool against databaseURL.
// Callers must call Close when done. Every connection gets shopspring/decimal
// registered as a native type, so ledger code (and anything else touching
// money) can use decimal.Decimal directly as a query param and scan target
// instead of float64 or manual string conversion.
func Open(ctx context.Context, databaseURL string) (*Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		pgxdecimal.Register(conn.TypeMap())
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return &Pool{pool}, nil
}
