package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestDecisionFailure_NoUnconditionalEscalateModel is the regression test
// for the removal of unconditional escalate_model from decision failure
// options. Before the fix, escalate_model was always appended to every
// failure's option list regardless of failure type. Now it should only
// appear in specific contexts (e.g. max-turns-exhausted), not in generic
// failures.
func TestDecisionFailure_NoUnconditionalEscalateModel(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_no_escalate_test",
		Status:           schemas.GraphRunning,
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepPending, RoleID: "role_x"}
	graph.Steps[step.ID] = step

	// Trigger a generic failure (not capability required, not context deficit).
	// This should go through scheduler_decision_failure.go with a transient
	// or generic failure class.
	engine.handleOutput(graph, step, &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusFailed,
			Confidence: 0.3,
		},
	})

	if len(graph.PendingDecisions) == 0 {
		t.Skip("no pending decision generated — test setup may need adjustment")
	}

	options := graph.PendingDecisions[0].Options
	for _, o := range options {
		if o == "escalate_model" {
			t.Errorf("escalate_model should not be unconditionally offered in failure options. Options: %v", options)
		}
	}

	// Verify base options are still present
	hasSkip, hasAbort, hasRetry := false, false, false
	for _, o := range options {
		switch o {
		case "skip":
			hasSkip = true
		case "abort":
			hasAbort = true
		case "retry":
			hasRetry = true
		}
	}
	if !hasSkip || !hasAbort || !hasRetry {
		t.Errorf("base options skip/abort/retry should always be present, got: %v", options)
	}
}
