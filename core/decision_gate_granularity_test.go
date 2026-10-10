package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestDecisionGate_NoGraphBlockedStatus verifies that addDecision does NOT
// set graph.Status to GraphBlocked. The graph stays GraphRunning; only the
// failing step is StepBlocked (by maybeBlock/handleDefaultFailure).
// This is the core invariant of step-level decision gate granularity.
func TestDecisionGate_NoGraphBlockedStatus(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-no-block",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepFailed}
	graph.Steps["s1"] = step

	s.Mu.Lock()
	s.graphs["task-no-block"] = graph
	s.Mu.Unlock()

	s.addDecision(graph, step, schemas.DecisionStepFailed,
		map[string]any{"last_error": "test"},
		[]string{"skip", "abort"})

	s.Mu.RLock()
	status := graph.Status
	pending := len(graph.PendingDecisions)
	s.Mu.RUnlock()

	if status == schemas.GraphBlocked {
		t.Fatalf("graph.Status should NOT be GraphBlocked (step-level blocking only), got %v", status)
	}
	if status != schemas.GraphRunning {
		t.Fatalf("expected GraphRunning, got %v", status)
	}
	if pending != 1 {
		t.Fatalf("expected 1 pending decision, got %d", pending)
	}
}

// TestDecisionGate_ParallelBranchesContinue verifies that when step A fails
// and creates a decision, an independent step B in the same graph can still
// be dispatched (its status is StepPending, not blocked by A's decision).
// This is the key benefit of step-level vs graph-level blocking.
func TestDecisionGate_ParallelBranchesContinue(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-parallel",
		Steps: map[string]*schemas.Step{
			"A": {ID: "A", Status: schemas.StepFailed, RoleID: "worker"},
			"B": {ID: "B", Status: schemas.StepPending, RoleID: "worker"},
		},
		Status: schemas.GraphRunning,
	}

	s.Mu.Lock()
	s.graphs["task-parallel"] = graph
	s.Mu.Unlock()

	// Step A fails → addDecision creates a pending decision.
	// With graph-level blocking, graph.Status would become GraphBlocked,
	// preventing B from being dispatched. With step-level blocking,
	// B remains StepPending and ReadySteps() includes it.
	s.addDecision(graph, graph.Steps["A"], schemas.DecisionStepFailed,
		map[string]any{"last_error": "A failed"},
		[]string{"skip", "abort"})

	s.Mu.RLock()
	status := graph.Status
	pending := len(graph.PendingDecisions)
	stepB := graph.Steps["B"]
	s.Mu.RUnlock()

	// Graph should still be running (not blocked).
	if status != schemas.GraphRunning {
		t.Fatalf("expected GraphRunning after A's decision, got %v", status)
	}

	// Decision should be pending for A.
	if pending != 1 {
		t.Fatalf("expected 1 pending decision, got %d", pending)
	}

	// Step B should still be StepPending (ready for dispatch).
	if stepB.Status != schemas.StepPending {
		t.Fatalf("expected step B to remain StepPending, got %v", stepB.Status)
	}

	// ReadySteps should include B (it has no dependencies and is pending).
	ready := graph.ReadySteps()
	foundB := false
	for _, r := range ready {
		if r.ID == "B" {
			foundB = true
			break
		}
	}
	if !foundB {
		t.Fatal("expected step B in ReadySteps() — it should be dispatchable despite A's decision")
	}
}

// TestDecisionGate_MultipleConcurrentDecisions verifies that two independent
// steps can fail simultaneously, creating two pending decisions. The graph
// stays GraphRunning. Resolving one decision does NOT require resolving the
// other — each is tracked independently.
func TestDecisionGate_MultipleConcurrentDecisions(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-multi-dec",
		Steps: map[string]*schemas.Step{
			"A": {ID: "A", Status: schemas.StepFailed, RoleID: "worker"},
			"B": {ID: "B", Status: schemas.StepFailed, RoleID: "worker"},
		},
		Status: schemas.GraphRunning,
	}

	s.Mu.Lock()
	s.graphs["task-multi-dec"] = graph
	s.Mu.Unlock()

	// Step A fails → decision 1
	s.addDecision(graph, graph.Steps["A"], schemas.DecisionStepFailed,
		map[string]any{"last_error": "A failed"},
		[]string{"skip", "abort"})

	// Step B fails → decision 2
	s.addDecision(graph, graph.Steps["B"], schemas.DecisionStepFailed,
		map[string]any{"last_error": "B failed"},
		[]string{"skip", "abort"})

	s.Mu.RLock()
	status := graph.Status
	pending := len(graph.PendingDecisions)
	s.Mu.RUnlock()

	// Graph should still be running.
	if status != schemas.GraphRunning {
		t.Fatalf("expected GraphRunning with 2 pending decisions, got %v", status)
	}

	// Both decisions should be pending.
	if pending != 2 {
		t.Fatalf("expected 2 pending decisions, got %d", pending)
	}

	// Resolve decision 1 (skip step A).
	decA := graph.PendingDecisions[0]
	if err := s.SubmitDecision("task-multi-dec", decA.ID, "skip"); err != nil {
		t.Fatalf("SubmitDecision for A failed: %v", err)
	}

	// Give async operations time to settle.
	time.Sleep(50 * time.Millisecond)

	s.Mu.RLock()
	pendingAfterResolve := len(graph.PendingDecisions)
	stepAStatus := graph.Steps["A"].Status
	stepBStatus := graph.Steps["B"].Status
	s.Mu.RUnlock()

	// Only decision 2 should remain pending.
	if pendingAfterResolve != 1 {
		t.Fatalf("expected 1 pending decision after resolving A, got %d", pendingAfterResolve)
	}

	// Step A should be skipped.
	if stepAStatus != schemas.StepSkipped {
		t.Fatalf("expected step A to be StepSkipped, got %v", stepAStatus)
	}

	// Step B should still be StepFailed (its decision is still pending).
	if stepBStatus != schemas.StepFailed {
		t.Fatalf("expected step B to still be StepFailed, got %v", stepBStatus)
	}
}
