package audit

import (
	"context"
	"fmt"
	"log"
	"time"
)

// RetentionJob periodically purges request_audit_log rows older than
// retention — ISP §7: "technical audit logs (request-level: IPs,
// timestamps, response codes) are retained for 12 months on a rolling
// basis". Migration 000026's own comment flagged this as a Phase 9 TODO.
// Ticker-driven, same shape as every other background job in this codebase.
type RetentionJob struct {
	logger       *Logger
	retention    time.Duration
	pollInterval time.Duration
}

func NewRetentionJob(logger *Logger, retention, pollInterval time.Duration) *RetentionJob {
	return &RetentionJob{logger: logger, retention: retention, pollInterval: pollInterval}
}

// Run blocks until ctx is cancelled, purging every pollInterval.
func (j *RetentionJob) Run(ctx context.Context) {
	ticker := time.NewTicker(j.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.logger.purgeOlderThan(ctx, time.Now().Add(-j.retention)); err != nil {
				log.Printf("audit: retention: %v", err)
			}
		}
	}
}

func (l *Logger) purgeOlderThan(ctx context.Context, cutoff time.Time) error {
	_, err := l.pool.Exec(ctx, `DELETE FROM request_audit_log WHERE created_at < $1`, cutoff)
	if err != nil {
		return fmt.Errorf("audit: purge older than %s: %w", cutoff, err)
	}
	return nil
}
