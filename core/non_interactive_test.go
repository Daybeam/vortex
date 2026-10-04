package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestDefaultNonInteractiveChoice is a regression test for E5: it verifies the
// conservative default chosen for each decision type in unattended mode.
func TestDefaultNonInteractiveChoice(t *testing.T) {
	cases := []struct {
		name    string
		dtype   schemas.DecisionType
		options []string
		want    string
		ok      bool
	}{
		{"step_failed → skip", schemas.DecisionStepFailed, []string{"skip", "abort"}, "skip", true},
		{"capability_required → skip", schemas.DecisionCapabilityRequired, []string{"skip", "abort"}, "skip", true},
		{"low_confidence → skip", schemas.DecisionLowConfidence, []string{"skip", "abort"}, "skip", true},
		{"environment_missing → skip", schemas.DecisionEnvironmentMissing, []string{"skip", "abort"}, "skip", true},
		{"delegation → skip", schemas.DecisionDelegationRequired, []string{"fulfill", "skip", "abort"}, "skip", true},
		{"upstream_insufficient → skip", schemas.DecisionUpstreamInsufficient, []string{"skip", "abort"}, "skip", true},
		{"budget_exhausted → skip", schemas.DecisionBudgetExhausted, []string{"skip", "abort"}, "skip", true},
		{"max_turns → resume", schemas.DecisionMaxTurnsExhausted, []string{"resume_more_turns", "skip", "abort"}, "resume_more_turns", true},
		{"autonomous_abort → continue", schemas.DecisionAutonomousAbortRequested, []string{"confirm_abort", "continue"}, "continue", true},
		// human_approval_required must NEVER auto-resolve (safety gate).
		{"human_approval → no auto", schemas.DecisionHumanApprovalRequired, []string{"approve", "reject"}, "", false},
		// rewrite_dag needs a surgery payload → not auto-resolvable.
		{"rewrite_dag → no auto", schemas.DecisionRewriteDAG, []string{"rewrite_dag", "skip"}, "", false},
		// Guard: default must be an offered option.
		{"skip not offered → no auto", schemas.DecisionStepFailed, []string{"abort"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := defaultNonInteractiveChoice(tc.dtype, tc.options)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("choice = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNonInteractive_AutoResolvesDecision verifies the end-to-end E5 path:
// when NonInteractive is enabled, a decision that would block forever is
// auto-resolved via SubmitDecision (async), leaving no pending decisions.
func TestNonInteractive_AutoResolvesDecision(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()

	// Enable unattended mode.
	reg.System.NonInteractive = true

	graph := &schemas.TaskGraph{
		TaskID: "task-ni-1",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-ni-1"] = graph
	s.Mu.Unlock()

	// RequestAutonomousAbort creates an autonomous_abort_requested decision
	// (options confirm_abort/continue). NonInteractive should auto-pick
	// "continue" → step resumes; run() then re-runs it, it fails, the
	// resulting step_failed decision auto-resolves to "skip". Net effect:
	// no decision remains pending and the graph is not stuck in GraphBlocked.
	s.RequestAutonomousAbort("task-ni-1", "s1", FailureClassCostOverrun, "test auto-resolve")

	// Auto-resolution is async (goroutine) — poll for the stable end state.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.Mu.RLock()
		pending := len(graph.PendingDecisions)
		blocked := graph.Status == schemas.GraphBlocked
		s.Mu.RUnlock()
		if pending == 0 && !blocked {
			return // success: no decision blocked the task
		}
		time.Sleep(20 * time.Millisecond)
	}

	s.Mu.RLock()
	pending := len(graph.PendingDecisions)
	blocked := graph.Status == schemas.GraphBlocked
	s.Mu.RUnlock()
	t.Fatalf("decision not auto-resolved: pending=%d blocked=%v", pending, blocked)
}

// TestNonInteractive_Disabled_StillBlocks verifies that when NonInteractive is
// false (the default), decisions block as before — zero behavior change.
func TestNonInteractive_Disabled_StillBlocks(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()

	if reg.System.NonInteractive {
		t.Fatal("NonInteractive should default to false")
	}

	graph := &schemas.TaskGraph{
		TaskID: "task-ni-2",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-ni-2"] = graph
	s.Mu.Unlock()

	s.RequestAutonomousAbort("task-ni-2", "s1", FailureClassCostOverrun, "should block")

	// Give any (erroneous) async auto-resolve a chance to fire.
	time.Sleep(100 * time.Millisecond)

	s.Mu.RLock()
	pending := len(graph.PendingDecisions)
	status := graph.Status
	s.Mu.RUnlock()
	if pending != 1 {
		t.Fatalf("expected 1 pending decision when NonInteractive=false, got %d", pending)
	}
	if status != schemas.GraphBlocked {
		t.Fatalf("expected GraphBlocked when NonInteractive=false, got %s", status)
	}
}
