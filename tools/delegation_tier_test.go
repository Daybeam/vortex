package tools

import (
	"strings"
	"testing"
)

// TestDelegationTierError_PublicRejected is the P0-3 regression test
// (DELEGATION_HANDOVER_REDESIGN.md §12.3): delegation mode returns the step's
// prompt for the CALLER to execute. The public tier has no execution path
// (orchestrator_run_command is admin-only, no workspace access), so accepting
// the submission would create a task that can never be fulfilled — and there
// is no task-level timeout to reclaim it (graph.TimeoutSecs is stored but never
// consumed).
func TestDelegationTierError_PublicRejected(t *testing.T) {
	err := delegationTierError(TierPublic)
	if err == nil {
		t.Fatal("public tier must be rejected in delegation mode; " +
			"otherwise it receives an envelope it cannot execute and the task blocks forever")
	}
	msg := err.Error()
	for _, want := range []string{"public-tier", "execution capability", "admin key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message should mention %q so the operator can act on it; got: %s", want, msg)
		}
	}
}

// Admin (local stdio / admin key) is the tier that actually executes
// delegated steps, so it must never be blocked.
func TestDelegationTierError_AdminAllowed(t *testing.T) {
	if err := delegationTierError(TierAdmin); err != nil {
		t.Fatalf("admin tier must be allowed to use delegation mode, got: %v", err)
	}
}

// An empty tier means the caller never traversed the auth middleware (background
// job / direct unit-test call). Treat it as non-public rather than rejecting —
// the same fallback EffectiveTier uses elsewhere.
func TestDelegationTierError_EmptyTierAllowed(t *testing.T) {
	if err := delegationTierError(""); err != nil {
		t.Fatalf("empty tier must not be rejected (no auth middleware in the path), got: %v", err)
	}
}
