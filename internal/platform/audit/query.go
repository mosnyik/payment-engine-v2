package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// LogEntry is one stored request_audit_log row — Record's fields plus the
// two the write path (audit.go) never needs: the row's own ID and when it
// was actually written. Kept as a separate type so Record, used on every
// request's hot path (platform/audit/middleware.go), never has to carry
// read-only fields it doesn't populate.
type LogEntry struct {
	Record
	ID        uuid.UUID
	CreatedAt time.Time
}

// LogFilter narrows List to one tenant and/or one admin — the two
// investigation questions this read surface exists for ("what did this
// tenant do", "what did this admin do"). Both nil means every row.
type LogFilter struct {
	TenantID *uuid.UUID
	AdminID  *uuid.UUID
}

// List is the admin browse surface (Phase 11) over request_audit_log —
// newest first. This table carries client IPs/user agents (ISP §7
// territory); it sits behind the same adminauth.Middleware gate as every
// other /admin route, since this codebase has no per-admin role/permission
// model to scope it further yet (a real, separate gap — see Phase 9's
// still-open tenant API-key permission-list item, and IMPLEMENTATION_PLAN.md
// Phase 11's own callout — not something to silently half-build here).
// limit<=0 means unbounded.
func (l *Logger) List(ctx context.Context, filter LogFilter, limit, offset int) ([]LogEntry, int, error) {
	var total int
	if err := l.pool.QueryRow(ctx,
		`SELECT count(*) FROM request_audit_log
		 WHERE ($1::uuid IS NULL OR tenant_id = $1) AND ($2::uuid IS NULL OR admin_id = $2)`,
		filter.TenantID, filter.AdminID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("audit: list: count: %w", err)
	}

	query := `SELECT id, request_id, method, path, action, resource_type, resource_id,
			client_ip, user_agent, body_hash, status_code, response_time_ms,
			tenant_id, api_key_id, admin_id, created_at
		FROM request_audit_log
		WHERE ($1::uuid IS NULL OR tenant_id = $1) AND ($2::uuid IS NULL OR admin_id = $2)
		ORDER BY created_at DESC`
	args := []any{filter.TenantID, filter.AdminID}
	if limit > 0 {
		query += ` LIMIT $3 OFFSET $4`
		args = append(args, limit, offset)
	}

	rows, err := l.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: list: %w", err)
	}
	defer rows.Close()

	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var resourceType, resourceID *string
		if err := rows.Scan(
			&e.ID, &e.RequestID, &e.Method, &e.Path, &e.Action, &resourceType, &resourceID,
			&e.ClientIP, &e.UserAgent, &e.BodyHash, &e.StatusCode, &e.ResponseTimeMS,
			&e.TenantID, &e.APIKeyID, &e.AdminID, &e.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("audit: list: scan: %w", err)
		}
		if resourceType != nil {
			e.ResourceType = *resourceType
		}
		if resourceID != nil {
			e.ResourceID = *resourceID
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("audit: list: %w", err)
	}
	return out, total, nil
}
