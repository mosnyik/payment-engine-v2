package ratelimit_test

import (
	"testing"
	"time"

	"github.com/sirfi/payment-engine-v2/internal/platform/ratelimit"
)

func TestLimiter_AllowsUpToLimitThenRejects(t *testing.T) {
	l := ratelimit.New(time.Minute)

	for i := range 3 {
		if !l.Allow("key", 3) {
			t.Fatalf("request %d: expected allowed", i)
		}
	}
	if l.Allow("key", 3) {
		t.Fatal("expected the 4th request within the same window to be rejected")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	l := ratelimit.New(time.Minute)

	for i := range 2 {
		if !l.Allow("a", 2) {
			t.Fatalf("key a request %d: expected allowed", i)
		}
	}
	if l.Allow("a", 2) {
		t.Fatal("expected key a to be rate limited")
	}
	if !l.Allow("b", 2) {
		t.Fatal("expected key b's budget to be unaffected by key a's usage")
	}
}

func TestLimiter_UnlimitedWhenLimitIsZeroOrLess(t *testing.T) {
	l := ratelimit.New(time.Minute)
	for i := range 100 {
		if !l.Allow("key", 0) {
			t.Fatalf("request %d: expected unlimited (limit=0) to always allow", i)
		}
	}
}

func TestLimiter_NilLimiterAlwaysAllows(t *testing.T) {
	var l *ratelimit.Limiter
	if !l.Allow("key", 1) {
		t.Fatal("expected a nil Limiter to always allow, matching every nil-safe optional dependency elsewhere in this codebase")
	}
}

func TestLimiter_WindowResetAllowsAgain(t *testing.T) {
	l := ratelimit.New(20 * time.Millisecond)

	if !l.Allow("key", 1) {
		t.Fatal("expected the first request to be allowed")
	}
	if l.Allow("key", 1) {
		t.Fatal("expected the second request within the same window to be rejected")
	}

	// Sleep past 2*window so the bucket resets fresh rather than blending
	// against a stale previous-window count.
	time.Sleep(50 * time.Millisecond)

	if !l.Allow("key", 1) {
		t.Fatal("expected a request after the window elapsed to be allowed again")
	}
}
