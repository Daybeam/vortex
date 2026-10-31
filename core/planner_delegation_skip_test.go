package core

import "testing"

// planner_delegation_skip_test.go — D5 regression test
// (DELEGATION_HANDOVER_REDESIGN.md §4.5, P1a).
//
// Bug: in delegation mode, the planner spawn gets short-circuited by
// tryDelegationMode, returns a prompt envelope instead of a plan, and
// silently falls back to caller-provided steps after a wasted retry.
// Fix: plannerFirstEnabled() returns false when DelegationMode is true.

// TestPlannerFirstEnabled_DelegationModeSkips verifies that planner-first
// is disabled in delegation mode, even when EnablePlannerFirst is true.
func TestPlannerFirstEnabled_DelegationModeSkips(t *testing.T) {
	s, reg := newPlannerTestEngine(t)

	// Enable planner-first AND delegation mode.
	reg.System.EnablePlannerFirst = true
	reg.System.DelegationMode = true

	// D5 fix: plannerFirstEnabled must return false in delegation mode.
	if s.plannerFirstEnabled() {
		t.Fatalf("plannerFirstEnabled must return false when DelegationMode is true")
	}

	// Sanity: without delegation mode, the flag should work normally.
	reg.System.DelegationMode = false
	if !s.plannerFirstEnabled() {
		t.Fatalf("plannerFirstEnabled must return true when DelegationMode is false and EnablePlannerFirst is true")
	}
}
