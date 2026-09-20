package config

import (
	"testing"
	"time"
)

// TestSetCooldown_IsCoolingDown_RoundTrip verifies the basic write/read cycle
// for the global provider rate-limit cooldown mechanism added 2026-08-16.
//
// FIX (2026-08-16/17, playbook addendum): SetCooldown was being called from
// core/scheduler.go on every 429 response, but nothing anywhere in the repo
// ever called IsCoolingDown to consult that state before selecting a
// provider -- the feature was entirely write-only. This test, together with
// the new consumer-side check added in core/spawner.go's candidate-selection
// loop, closes that gap. A full spawner-level integration test would need
// substantial provider/MCP mock scaffolding (matching the documented
// cost/benefit tradeoff noted for ensureMCPClient in the 08-07 addendum),
// so this test is scoped to the Registry-level contract only.
func TestSetCooldown_IsCoolingDown_RoundTrip(t *testing.T) {
	r := &Registry{}

	// Before any cooldown is set, the provider should not be cooling down.
	if cooling, remaining := r.IsCoolingDown("gemini"); cooling {
		t.Fatalf("expected no cooldown before SetCooldown, got cooling=%v remaining=%v", cooling, remaining)
	}

	r.SetCooldown("gemini", 2*time.Second)

	cooling, remaining := r.IsCoolingDown("gemini")
	if !cooling {
		t.Fatalf("expected cooling=true immediately after SetCooldown")
	}
	if remaining <= 0 || remaining > 2*time.Second {
		t.Fatalf("expected remaining in (0, 2s], got %v", remaining)
	}

	// A different, unrelated provider ID must not be affected.
	if cooling, _ := r.IsCoolingDown("gemma"); cooling {
		t.Fatalf("expected unrelated provider ID 'gemma' to be unaffected by SetCooldown(\"gemini\", ...)")
	}
}

// TestSetCooldown_ClearedByRecordSuccess verifies that the existing
// success-report path (RecordSuccess, called by RetryingProvider's
// self-reporting on a successful call per providers/retry.go) resets
// CooldownUntil back to zero, so a provider that recovers before its
// cooldown window naturally elapses is not left stuck as "cooling down".
func TestSetCooldown_ClearedByRecordSuccess(t *testing.T) {
	r := &Registry{}
	r.SetCooldown("openai", 5*time.Second)

	if cooling, _ := r.IsCoolingDown("openai"); !cooling {
		t.Fatalf("expected cooling=true right after SetCooldown")
	}

	r.RecordSuccess("openai")

	if cooling, remaining := r.IsCoolingDown("openai"); cooling {
		t.Fatalf("expected cooling=false after RecordSuccess, got cooling=%v remaining=%v", cooling, remaining)
	}
}

// TestSetCooldown_ExpiresNaturally verifies IsCoolingDown correctly reports
// false once the cooldown duration has elapsed, without requiring an
// explicit clear.
func TestSetCooldown_ExpiresNaturally(t *testing.T) {
	r := &Registry{}
	r.SetCooldown("gemini", 10*time.Millisecond)

	time.Sleep(30 * time.Millisecond)

	if cooling, remaining := r.IsCoolingDown("gemini"); cooling {
		t.Fatalf("expected cooldown to have expired naturally, got cooling=%v remaining=%v", cooling, remaining)
	}
}
