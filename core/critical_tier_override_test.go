package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Regression tests for Critical Tier Auto-Resolve Override.
// See docs/completed/2026-10-17/CRITICAL_TIER_AUTO_RESOLVE_OVERRIDE_DESIGN.md.
//
// These tests verify:
//  1. Default behavior is preserved (critical tier still blocks when override is off).
//  2. AllowCriticalAutoResolve=true unblocks the deadlock.
//  3. human_approval_required returns "" when AllowHumanApprovalAutoResolve=false.
//  4. human_approval_required returns "abort" when AllowHumanApprovalAutoResolve=true.
//  5. defaultNonInteractiveChoice picks "abort" for human_approval when abort is offered.

// ── Critical tier override ──────────────────────────────────────────────

// TestCriticalTierOverride_Default_Blocks verifies that when
// AllowCriticalAutoResolve is false (the default), a RiskTierCritical
// decision in NonInteractive mode stays pending (deadlock preserved —
// zero behavior change from pre-override behavior).
func TestCriticalTierOverride_Default_Blocks(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()

	reg.System.NonInteractive = true
	// AllowCriticalAutoResolve defaults false — no override.

	graph := &schemas.TaskGraph{
		TaskID: "task-crit-default",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	// "deploy" in Task → DetermineRiskTier returns RiskTierCritical.
	step := &schemas.Step{ID: "s1", Task: "deploy to production", Status: schemas.StepFailed}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-crit-default"] = graph
	s.Mu.Unlock()

	s.addDecision(graph, step, schemas.DecisionStepFailed,
		map[string]any{"root_cause": "test"}, []string{"skip", "abort"})

	// Give any (erroneous) async auto-resolve a chance to fire.
	time.Sleep(100 * time.Millisecond)

	s.Mu.RLock()
	pending := len(graph.PendingDecisions)
	s.Mu.RUnlock()
	if pending != 1 {
		t.Fatalf("expected 1 pending decision (critical tier must block when "+
			"AllowCriticalAutoResolve=false), got %d", pending)
	}
}

// TestCriticalTierOverride_Enabled_AutoResolves verifies that when
// AllowCriticalAutoResolve is true, a RiskTierCritical decision in
// NonInteractive mode is auto-resolved instead of hanging forever.
func TestCriticalTierOverride_Enabled_AutoResolves(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()

	reg.System.NonInteractive = true
	reg.System.AllowCriticalAutoResolve = true

	graph := &schemas.TaskGraph{
		TaskID: "task-crit-override",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Task: "deploy to production", Status: schemas.StepFailed}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-crit-override"] = graph
	s.Mu.Unlock()

	s.addDecision(graph, step, schemas.DecisionStepFailed,
		map[string]any{"root_cause": "test"}, []string{"skip", "abort"})

	// Auto-resolution is async — poll for the stable end state.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.Mu.RLock()
		pending := len(graph.PendingDecisions)
		s.Mu.RUnlock()
		if pending == 0 {
			return // success: critical decision was auto-resolved
		}
		time.Sleep(20 * time.Millisecond)
	}

	s.Mu.RLock()
	pending := len(graph.PendingDecisions)
	s.Mu.RUnlock()
	t.Fatalf("critical decision not auto-resolved with AllowCriticalAutoResolve=true: "+
		"pending=%d (deadlock not unblocked)", pending)
}

// ── Human approval override ─────────────────────────────────────────────

// TestHumanApprovalOverride_Default_ReturnsEmpty verifies that when
// AllowHumanApprovalAutoResolve is false (the default),
// resolveNonInteractiveDecision returns "" for human_approval_required
// (must wait for a human — legal/compliance gate).
//
// Options include "abort" deliberately: without the guard, the conservative
// default would pick "abort" and auto-resolve. The guard is the ONLY thing
// preventing auto-resolution. This makes the test mutation-sensitive.
func TestHumanApprovalOverride_Default_ReturnsEmpty(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()

	reg.System.NonInteractive = true
	// AllowHumanApprovalAutoResolve defaults false.

	choice := s.resolveNonInteractiveDecision(
		"", "task-ha-default", "dec1", "s1",
		schemas.DecisionHumanApprovalRequired,
		map[string]any{}, []string{"abort", "approve"},
		false,
	)
	if choice != "" {
		t.Fatalf("expected empty choice for human_approval_required when "+
			"AllowHumanApprovalAutoResolve=false, got %q", choice)
	}
}

// TestHumanApprovalOverride_Enabled_ReturnsAbort verifies that when
// AllowHumanApprovalAutoResolve is true, resolveNonInteractiveDecision
// resolves human_approval_required to the conservative "abort" default.
func TestHumanApprovalOverride_Enabled_ReturnsAbort(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()

	reg.System.NonInteractive = true
	reg.System.AllowHumanApprovalAutoResolve = true

	choice := s.resolveNonInteractiveDecision(
		"", "task-ha-override", "dec1", "s1",
		schemas.DecisionHumanApprovalRequired,
		map[string]any{}, []string{"abort", "approve"},
		false,
	)
	if choice != "abort" {
		t.Fatalf("expected conservative 'abort' for human_approval_required "+
			"when AllowHumanApprovalAutoResolve=true, got %q", choice)
	}
}

// ── defaultNonInteractiveChoice human_approval cases ───────────────────

// TestDefaultNonInteractiveChoice_HumanApproval_Override verifies the
// conservative default for human_approval_required when the override is on:
// prefers "abort", falls back to "skip", returns ("",false) if neither offered.
func TestDefaultNonInteractiveChoice_HumanApproval_Override(t *testing.T) {
	cases := []struct {
		name    string
		options []string
		want    string
		ok      bool
	}{
		{"abort offered → abort", []string{"abort", "approve"}, "abort", true},
		{"skip fallback", []string{"approve", "skip"}, "skip", true},
		{"neither offered → no auto", []string{"approve", "reject"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := defaultNonInteractiveChoice(schemas.DecisionHumanApprovalRequired, tc.options)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("choice = %q, want %q", got, tc.want)
			}
		})
	}
}
