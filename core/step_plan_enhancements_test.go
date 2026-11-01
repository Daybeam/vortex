package core

import (
	"testing"
)

// Sieve-only coverage (#4 plan drift, #13 adaptive threshold, #16 cache
// backpressure) moved with the package to
// core/sieve/sieve_plan_threshold_cache_test.go.

// ─── #10: PendingSteer ─────────────────────────────────────────────────────

func TestChatHarness_PendingSteer_DefaultEmpty(t *testing.T) {
	h := &ChatHarness{}
	h.SteerLock.Lock()
	if h.PendingSteer != "" {
		t.Error("PendingSteer should default to empty")
	}
	h.SteerLock.Unlock()
}
