// Package idempotency guards the inbound API boundary: "did this tenant
// already POST this request". Distinct from the ledger's own idempotency
// keys, which guard internal money-movement claims, not inbound requests.
package idempotency

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

// Outcome describes what Reserve found for a given key.
type Outcome int

const (
	// Claimed means this call is the first to use the key — the caller owns
	// it and should proceed with the request, then call Complete.
	Claimed Outcome = iota
	// InFlight means another request with this key is still being
	// processed. The caller should reject this request (e.g. 409) rather
	// than proceed or wait indefinitely.
	InFlight
	// Completed means a prior request with this key already finished —
	// CachedResponse holds what was returned then; the caller should
	// replay it rather than re-execute anything.
	Completed
)

type CachedResponse struct {
	Code int
	Body []byte
}

type Store struct {
	pool *db.Pool
}

func New(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// Reserve attempts to atomically claim key for tenantID.
func (s *Store) Reserve(ctx context.Context, tenantID uuid.UUID, key string) (Outcome, *CachedResponse, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO idempotency_keys (tenant_id, key) VALUES ($1, $2)
		 ON CONFLICT (tenant_id, key) DO NOTHING`,
		tenantID, key,
	)
	if err != nil {
		return 0, nil, fmt.Errorf("idempotency: reserve: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return Claimed, nil, nil
	}

	var responseCode *int
	var responseBody []byte
	err = s.pool.QueryRow(ctx,
		`SELECT response_code, response_body FROM idempotency_keys WHERE tenant_id = $1 AND key = $2`,
		tenantID, key,
	).Scan(&responseCode, &responseBody)
	if err != nil {
		return 0, nil, fmt.Errorf("idempotency: check existing: %w", err)
	}

	if responseCode == nil {
		return InFlight, nil, nil
	}
	return Completed, &CachedResponse{Code: *responseCode, Body: responseBody}, nil
}

// Complete records the response for a previously-reserved key so future
// replays return it instead of re-executing the request.
func (s *Store) Complete(ctx context.Context, tenantID uuid.UUID, key string, responseCode int, responseBody []byte) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE idempotency_keys SET response_code = $3, response_body = $4
		 WHERE tenant_id = $1 AND key = $2`,
		tenantID, key, responseCode, responseBody,
	)
	if err != nil {
		return fmt.Errorf("idempotency: complete: %w", err)
	}
	return nil
}
