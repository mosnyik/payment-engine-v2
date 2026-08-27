// Package ratelimit implements the sliding-window request-rate limiting
// ISP §6 requires ("100/1,000/10,000 req/min by tier, sliding window"). This
// is in-process/in-memory — the system runs as a single binary today
// (ARCHITECTURE.md's modular-monolith framing) with no shared external
// store (Redis etc.) yet; a multi-instance deployment would need to swap
// this for one, but nothing currently calls for it.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter is a sliding-window-counter rate limiter keyed by an arbitrary
// string (a client IP, a tenant ID, ...). It approximates a true sliding
// window by blending the previous and current fixed window's counts,
// weighted by how far into the current window "now" is — O(1) per Allow
// call and no per-request timestamp history to store or prune, unlike a
// sliding-window log.
type Limiter struct {
	window time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	windowStart time.Time
	prevCount   int
	currCount   int
}

// New builds a Limiter measuring requests per window (e.g. time.Minute, to
// match ISP §6's "req/min" framing).
func New(window time.Duration) *Limiter {
	return &Limiter{window: window, buckets: make(map[string]*bucket)}
}

// Allow reports whether one more request under key is allowed within limit
// per window, and records it if so. limit <= 0 means unlimited (always
// allowed) — callers pass this for a nil Limiter or an unrecognized tier
// rather than special-casing it themselves.
func (l *Limiter) Allow(key string, limit int) bool {
	if l == nil || limit <= 0 {
		return true
	}

	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{windowStart: now}
		l.buckets[key] = b
	}

	elapsed := now.Sub(b.windowStart)
	switch {
	case elapsed >= 2*l.window:
		// Idle long enough that even a blended previous-window count is
		// meaningless — start fresh rather than carrying stale history.
		b.windowStart = now
		b.prevCount = 0
		b.currCount = 0
		elapsed = 0
	case elapsed >= l.window:
		b.windowStart = b.windowStart.Add(l.window)
		b.prevCount = b.currCount
		b.currCount = 0
		elapsed = now.Sub(b.windowStart)
	}

	weight := 1 - float64(elapsed)/float64(l.window)
	estimate := float64(b.prevCount)*weight + float64(b.currCount)
	if estimate >= float64(limit) {
		return false
	}
	b.currCount++
	return true
}

// Run periodically evicts buckets idle for more than 2*window — without
// this, a limiter keyed by client IP or tenant ID grows forever as new keys
// are seen. Ticker-driven, same shape as every other background job in this
// codebase (e.g. session.TTLJob). Safe to not start at all: Allow works
// without it, just without eviction.
func (l *Limiter) Run(ctx context.Context) {
	ticker := time.NewTicker(l.window)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.sweep()
		}
	}
}

func (l *Limiter) sweep() {
	cutoff := time.Now().Add(-2 * l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, b := range l.buckets {
		if b.windowStart.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
}
