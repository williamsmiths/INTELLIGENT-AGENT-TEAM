package api

import (
	"fmt"
	"testing"
	"time"
)

// ATTEST_REJECTED is journaled at most once a minute per pid and reason, and
// the memory of the limiter stays bounded.
func TestRateLimitOncePerMinutePerKey(t *testing.T) {
	var l rateLimit
	if !l.allow("42/no_host") || l.allow("42/no_host") || l.allow("42/no_host") {
		t.Fatal("the same pid and reason was let through twice within a minute")
	}
	if !l.allow("42/not_enrolled") || !l.allow("43/no_host") {
		t.Fatal("another reason or another pid was held back")
	}
	l.last["42/no_host"] = time.Now().Add(-61 * time.Second)
	if !l.allow("42/no_host") {
		t.Fatal("not let through again after a minute")
	}
	for i := range 5000 {
		l.last[fmt.Sprintf("old-%d", i)] = time.Now().Add(-2 * time.Minute)
	}
	if !l.allow("new") || len(l.last) > 16 {
		t.Fatalf("old entries were not forgotten: %d kept", len(l.last))
	}
}
