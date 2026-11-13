package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestEvalC6_TerminalStateGuardInDecisionGate verifies the P0 fix (eval C6):
// addDecision must NOT transition a terminal-state graph back to GraphBlocked.
// Before the fix, cancelling a task while a decision was being enqueued would
// silently lose the cancellation (Cancelled → Blocked). After the fix, the
// terminal state is preserved.
func TestEvalC6_TerminalStateGuardInDecisionGate(t *testing.T) {
	engine, _ := newDelegationEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_1", RoleID: "worker", Task: "test terminal guard"},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait for graph to exist.
	var graph *schemas.TaskGraph
	for i := 0; i < 20; i++ {
		graph = engine.GetTaskGraph(taskID)
		if graph != nil {
			break
		}
	}
	if graph == nil {
		t.Fatal("graph not registered after Submit")
	}

	// Cancel the task — puts graph into GraphCancelled (terminal).
	if err := engine.CancelTask(taskID); err != nil {
		t.Fatalf("CancelTask failed: %v", err)
	}

	graph = engine.GetTaskGraph(taskID)
	if graph.Status != schemas.GraphCancelled {
		t.Fatalf("expected GraphCancelled, got %v", graph.Status)
	}

	// Now call addDecision directly on the cancelled graph.
	// Before P0 fix: this would set Status = GraphBlocked, losing the cancellation.
	// After P0 fix: the terminal-state guard returns early, preserving GraphCancelled.
	step := &schemas.Step{ID: "step_1"}
	engine.addDecision(graph, step, schemas.DecisionStepFailed,
		map[string]any{"last_error": "test"},
		[]string{"skip", "abort"})

	graph = engine.GetTaskGraph(taskID)
	finalStatus := graph.Status
	pendingCount := len(graph.PendingDecisions)

	if finalStatus != schemas.GraphCancelled {
		t.Errorf("P0 REGRESSION: addDecision changed terminal status from Cancelled to %v "+
			"(terminal-state guard missing)", finalStatus)
	}
	if pendingCount != 0 {
		t.Errorf("P0 REGRESSION: addDecision appended a pending decision to a terminal graph "+
			"(got %d pending)", pendingCount)
	}
}

// TestEvalS05_ParseFailureFallbackTriggersSpinDetection verifies the P1 fix
// (eval §0.5): ParseOutput's fallback must produce Assumptions containing
// "not parseable" so the spawner's spin detector can identify parse failures
// and break after maxConsecutiveParseFailures instead of burning 19-43 turns.
func TestEvalS05_ParseFailureFallbackTriggersSpinDetection(t *testing.T) {
	// Garbage input that ParseOutput cannot parse → fallback path.
	garbage := "This is not JSON. It is just plain text with no structure."
	output := schemas.ParseOutput(garbage, "text")

	if output.Status != schemas.StatusPartial {
		t.Errorf("expected StatusPartial for unparseable input, got %v", output.Status)
	}
	if output.Confidence != 0.3 {
		t.Errorf("expected Confidence=0.3 for fallback, got %v", output.Confidence)
	}

	// The spin detector checks Assumptions for "not parseable".
	foundParseFailure := false
	for _, a := range output.Assumptions {
		if strings.Contains(a, "not parseable") || strings.Contains(a, "semantically hollow") {
			foundParseFailure = true
			break
		}
	}
	if !foundParseFailure {
		t.Errorf("P1 REGRESSION: ParseOutput fallback did not set parse-failure Assumption "+
			"(got %v); spin detector cannot identify parse failures without this", output.Assumptions)
	}
}

// TestEvalS05_SemanticallyHollowTriggersSpinDetection verifies the other
// fallback branch: output is valid JSON but missing confidence/result fields.
func TestEvalS05_SemanticallyHollowTriggersSpinDetection(t *testing.T) {
	// Valid JSON but missing required fields → "semantically hollow" fallback.
	hollowJSON := `{"some_field": "value but no status/confidence/result"}`
	output := schemas.ParseOutput(hollowJSON, "text")

	if output.Status != schemas.StatusPartial {
		t.Errorf("expected StatusPartial for semantically hollow input, got %v", output.Status)
	}

	foundHollow := false
	for _, a := range output.Assumptions {
		if strings.Contains(a, "semantically hollow") || strings.Contains(a, "not parseable") {
			foundHollow = true
			break
		}
	}
	if !foundHollow {
		t.Errorf("P1 REGRESSION: ParseOutput fallback did not set hollow/parse-failure Assumption "+
			"(got %v); spin detector cannot identify this failure mode", output.Assumptions)
	}
}

// TestEvalS05_ValidOutputDoesNotTriggerSpinDetection verifies that properly
// formatted output does NOT trigger the spin detector — only failures do.
// This prevents false positives that would break normal operation.
func TestEvalS05_ValidOutputDoesNotTriggerSpinDetection(t *testing.T) {
	validJSON := `{"status": "ok", "confidence": 0.9, "result": {"answer": "42"}}`
	output := schemas.ParseOutput(validJSON, "text")

	if output.Status != schemas.StatusOK {
		t.Errorf("expected StatusOK for valid input, got %v", output.Status)
	}

	for _, a := range output.Assumptions {
		if strings.Contains(a, "not parseable") || strings.Contains(a, "semantically hollow") {
			t.Errorf("P1 FALSE POSITIVE: valid output triggered parse-failure detection "+
				"(Assumption: %s); this would falsely activate the spin breaker", a)
		}
	}
}
