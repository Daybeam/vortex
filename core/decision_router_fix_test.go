package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestBrokenRouterRemoved_GenerativeUncertaintyEnqueuesDecision is a regression
// test for the removed "Autonomous Decision Router" (scheduler_decision_gate.go
// lines 90-100, removed 2026-10-02).
//
// BEFORE: addDecision with DecisionStepFailed + root_cause="generative_uncertainty"
// was intercepted by the broken router, which called SubmitDecision BEFORE the
// decision was enqueued (always failing silently) and returned early — leaving
// the step blocked with no pending decision to resolve it (silent deadlock).
//
// AFTER: the decision is properly enqueued in PendingDecisions and the graph
// is marked GraphBlocked, regardless of root_cause. The E5 NonInteractive
// auto-resolver (if enabled) handles unattended resolution.
func TestBrokenRouterRemoved_GenerativeUncertaintyEnqueuesDecision(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-router-fix",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepFailed}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-router-fix"] = graph
	s.Mu.Unlock()

	// This is the exact call pattern from handleStepFailure (line 153):
	// DecisionStepFailed with root_cause in context. The broken router
	// intercepted this exact shape and silently dropped it.
	s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
		"root_cause":       "generative_uncertainty",
		"suggested_action": "retry",
		"confidence":       0.3,
	}, []string{"skip", "abort", "retry", "refine_and_retry"})

	s.Mu.RLock()
	pending := len(graph.PendingDecisions)
	status := graph.Status
	s.Mu.RUnlock()

	if pending != 1 {
		t.Fatalf("expected 1 pending decision (broken router would have dropped it), got %d", pending)
	}
	if status != schemas.GraphRunning {
		t.Fatalf("expected GraphRunning (step-level blocking, not graph-level), got %s", status)
	}
}

// TestBrokenRouterRemoved_NonGenerativeUncertaintyStillEnqueues verifies that
// non-generative_uncertainty step_failed decisions were never affected by the
// broken router and still enqueue correctly (control case).
func TestBrokenRouterRemoved_NonGenerativeUncertaintyStillEnqueues(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	for _, rootCause := range []string{"context_deficit", "capability_required", "rate_limit"} {
		graph := &schemas.TaskGraph{
			TaskID: "task-ctrl-" + rootCause,
			Steps:  make(map[string]*schemas.Step),
			Status: schemas.GraphRunning,
		}
		step := &schemas.Step{ID: "s1", Status: schemas.StepFailed}
		graph.Steps["s1"] = step

		s.Mu.Lock()
		s.graphs[graph.TaskID] = graph
		s.Mu.Unlock()

		s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
			"root_cause": rootCause,
		}, []string{"skip", "abort", "retry"})

		s.Mu.RLock()
		pending := len(graph.PendingDecisions)
		s.Mu.RUnlock()

		if pending != 1 {
			t.Fatalf("root_cause=%s: expected 1 pending, got %d", rootCause, pending)
		}
	}
}

// TestNonInteractive_DelegateFallbackToConservative verifies the E5 delegate
// strategy: when DecisionDeciderRole is set but the spawn fails (no provider
// configured in test), the engine falls back to conservative defaults rather
// than leaving the decision blocked.
func TestNonInteractive_DelegateFallbackToConservative(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()

	reg.System.NonInteractive = true
	reg.System.DecisionDeciderRole = "decision-arbiter"

	graph := &schemas.TaskGraph{
		TaskID: "task-delegate-fallback",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-delegate-fallback"] = graph
	s.Mu.Unlock()

	// RequestAutonomousAbort creates a decision with options confirm_abort/continue.
	// The delegate strategy will try to spawn "decision-arbiter" but the test
	// spawner has no provider → spawn fails → falls back to conservative →
	// picks "continue". Net effect: no decision remains pending.
	s.RequestAutonomousAbort("task-delegate-fallback", "s1", FailureClassCostOverrun, "test delegate fallback")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.Mu.RLock()
		pending := len(graph.PendingDecisions)
		blocked := len(graph.PendingDecisions) > 0
		s.Mu.RUnlock()
		if pending == 0 && !blocked {
			return // success: delegate failed → conservative fallback resolved it
		}
		time.Sleep(20 * time.Millisecond)
	}

	s.Mu.RLock()
	pending := len(graph.PendingDecisions)
	blocked := len(graph.PendingDecisions) > 0
	s.Mu.RUnlock()
	t.Fatalf("delegate fallback failed: pending=%d blocked=%v", pending, blocked)
}

// TestNonInteractive_NoDeciderRole_UsesConservative verifies that when
// DecisionDeciderRole is empty, the conservative strategy is used directly
// (no spawn attempt). This is the default E5 behavior.
func TestNonInteractive_NoDeciderRole_UsesConservative(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()

	reg.System.NonInteractive = true
	// DecisionDeciderRole intentionally left empty.

	graph := &schemas.TaskGraph{
		TaskID: "task-no-decider",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-no-decider"] = graph
	s.Mu.Unlock()

	s.RequestAutonomousAbort("task-no-decider", "s1", FailureClassCostOverrun, "test conservative only")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.Mu.RLock()
		pending := len(graph.PendingDecisions)
		blocked := len(graph.PendingDecisions) > 0
		s.Mu.RUnlock()
		if pending == 0 && !blocked {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	s.Mu.RLock()
	pending := len(graph.PendingDecisions)
	s.Mu.RUnlock()
	t.Fatalf("conservative strategy failed: pending=%d", pending)
}
