// Package db provides the shared Postgres connection pool used by every module.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgx connection pool. All modules take a *Pool, never open
// their own connections, so pooling/config lives in exactly one place.
type Pool struct {
	*pgxpool.Pool
}

// Open creates and validates a connection pool against databaseURL.
// Callers must call Close when done.
func Open(ctx context.Context, databaseURL string) (*Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}

	return &Pool{pool}, nil
}
