package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func newTestEngineForFailurePolicy(t *testing.T) *DirectedEngine {
	t.Helper()
	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })
	return &DirectedEngine{logger: logger}
}

func TestApplyStepFailurePolicy_NilPolicyFallsThrough(t *testing.T) {
	engine := newTestEngineForFailurePolicy(t)
	graph := &schemas.TaskGraph{Status: schemas.GraphRunning}
	step := &schemas.Step{ID: "s1", Status: schemas.StepBlocked}

	applied := engine.applyStepFailurePolicy(graph, step, "task1", FailureClassRoleMissing)

	if applied {
		t.Fatal("expected applyStepFailurePolicy to return false when step.FailurePolicy is nil")
	}
	// Neither graph nor step status should have been touched -- caller is
	// expected to fall through to the existing decision_required behavior.
	if graph.Status != schemas.GraphRunning {
		t.Fatalf("expected graph.Status untouched (GraphRunning), got %v", graph.Status)
	}
	if step.Status != schemas.StepBlocked {
		t.Fatalf("expected step.Status untouched (StepBlocked), got %v", step.Status)
	}
}

func TestApplyStepFailurePolicy_UnrecognizedOnFailureFallsThrough(t *testing.T) {
	engine := newTestEngineForFailurePolicy(t)
	graph := &schemas.TaskGraph{Status: schemas.GraphRunning}
	step := &schemas.Step{
		ID:            "s1",
		Status:        schemas.StepBlocked,
		FailurePolicy: &schemas.FailurePolicy{OnFailure: "retry_forever_somehow"},
	}

	applied := engine.applyStepFailurePolicy(graph, step, "task1", FailureClassRoleMissing)

	if applied {
		t.Fatal("expected applyStepFailurePolicy to return false for an unrecognized OnFailure value")
	}
	if graph.Status != schemas.GraphRunning {
		t.Fatalf("expected graph.Status untouched, got %v", graph.Status)
	}
}

func TestApplyStepFailurePolicy_Abort(t *testing.T) {
	engine := newTestEngineForFailurePolicy(t)
	// Mirror the real call sites: graph.Status is already GraphBlocked and
	// step.Status is already StepBlocked by the time this is called.
	graph := &schemas.TaskGraph{Status: schemas.GraphBlocked}
	step := &schemas.Step{
		ID:            "s1",
		Status:        schemas.StepBlocked,
		FailurePolicy: &schemas.FailurePolicy{OnFailure: "abort"},
	}

	applied := engine.applyStepFailurePolicy(graph, step, "task1", FailureClassRoleMissing)

	if !applied {
		t.Fatal("expected applyStepFailurePolicy to return true for OnFailure=abort")
	}
	// Must match SubmitDecision's own "abort" case exactly: graph.Status ->
	// GraphFailed, step.Status left as-is (already StepBlocked).
	if graph.Status != schemas.GraphFailed {
		t.Fatalf("expected graph.Status == GraphFailed, got %v", graph.Status)
	}
	if step.Status != schemas.StepBlocked {
		t.Fatalf("expected step.Status left as StepBlocked (unchanged by abort), got %v", step.Status)
	}
}

func TestApplyStepFailurePolicy_Skip(t *testing.T) {
	engine := newTestEngineForFailurePolicy(t)
	graph := &schemas.TaskGraph{Status: schemas.GraphBlocked}
	step := &schemas.Step{
		ID:            "s1",
		Status:        schemas.StepBlocked,
		FailurePolicy: &schemas.FailurePolicy{OnFailure: "skip"},
	}

	applied := engine.applyStepFailurePolicy(graph, step, "task1", FailureClassMissingDependency)

	if !applied {
		t.Fatal("expected applyStepFailurePolicy to return true for OnFailure=skip")
	}
	// Must match SubmitDecision's own "skip" case exactly: step.Status ->
	// StepSkipped, graph.Status untouched by this call (the caller/engine's
	// normal graph-progression logic decides what happens to the graph
	// once a step is skipped, same as it does for a real submitted "skip"
	// decision).
	if step.Status != schemas.StepSkipped {
		t.Fatalf("expected step.Status == StepSkipped, got %v", step.Status)
	}
	if graph.Status != schemas.GraphBlocked {
		t.Fatalf("expected graph.Status left untouched by the skip case, got %v", graph.Status)
	}
}
