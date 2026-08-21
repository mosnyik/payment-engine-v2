// Package audit is the blanket per-request audit log ISP §7 requires: every
// API request (except health checks) is recorded with enough detail to
// answer "who did what, from where, with what result" — separate from
// adminauth's admin_audit_log, which only covers admin actions a handler
// explicitly calls LogAction for.
package audit

import (
	"context"
	"fmt"
	"log"

	"github.com/google/uuid"

	"github.com/sirfi/payment-engine-v2/internal/platform/db"
)

// Record is one logged request. BodyHash, never the raw body, per ISP §5's
// handling rule for anything that might carry a secret/PII payload.
type Record struct {
	RequestID      string
	Method         string
	Path           string
	Action         string
	ResourceType   string
	ResourceID     string
	ClientIP       string
	UserAgent      string
	BodyHash       string
	StatusCode     int
	ResponseTimeMS int64
	TenantID       *uuid.UUID
	APIKeyID       *uuid.UUID
	AdminID        *uuid.UUID
}

// bufferSize is a fixed backpressure valve, not an operational tuning knob
// a deployment would ever need to change — unlike the poll intervals in
// config.Config, it has no meaningful "right" value to configure per-env.
const bufferSize = 1024

// Logger buffers records in memory and writes them from a single background
// goroutine (Run), so a slow or momentarily-unavailable database never adds
// latency to the request path — ISP §7's "logging is asynchronous and does
// not block the response path".
type Logger struct {
	pool *db.Pool
	ch   chan Record
}

func New(pool *db.Pool) *Logger {
	return &Logger{pool: pool, ch: make(chan Record, bufferSize)}
}

// Log enqueues rec without blocking the caller. If the buffer is full — the
// database is falling behind or down — the record is dropped rather than
// piling up latency on the request that generated it.
func (l *Logger) Log(rec Record) {
	select {
	case l.ch <- rec:
	default:
		log.Printf("audit: buffer full, dropping record for %s %s", rec.Method, rec.Path)
	}
}

// Run drains the buffer until ctx is cancelled, writing one row per record.
// Started as a background goroutine from main.go, the same shape as every
// other *Job/*Worker.Run(ctx) in this codebase.
func (l *Logger) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case rec := <-l.ch:
			if err := l.write(ctx, rec); err != nil {
				log.Printf("audit: write record: %v", err)
			}
		}
	}
}

func (l *Logger) write(ctx context.Context, rec Record) error {
	_, err := l.pool.Exec(ctx,
		`INSERT INTO request_audit_log
			(request_id, method, path, action, resource_type, resource_id,
			 client_ip, user_agent, body_hash, status_code, response_time_ms,
			 tenant_id, api_key_id, admin_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		rec.RequestID, rec.Method, rec.Path, rec.Action, nullIfEmpty(rec.ResourceType), nullIfEmpty(rec.ResourceID),
		rec.ClientIP, rec.UserAgent, rec.BodyHash, rec.StatusCode, rec.ResponseTimeMS,
		rec.TenantID, rec.APIKeyID, rec.AdminID,
	)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
