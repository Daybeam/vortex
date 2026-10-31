package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestFulfillStep_SingleStepOverlayMerge is a regression test for D3 fast-path
// (DELEGATION_HANDOVER_REDESIGN.md §10.2): the single-step fast-path in
// scheduler_submit.go:321-341 created a Step literal without copying
// MergeStrategy/TargetKey from the StepInput. This caused the overlay merge
// in scheduler_decision_success.go:47 to silently skip (both fields were ""),
// so GlobalWorkspace[target_key] was never set by the merge — only by
// AutoDepositResult under the step ID.
//
// Before the fix: GlobalWorkspace["answer"] is missing (only ["step_1"] exists).
// After  the fix: GlobalWorkspace["answer"] is set by the overlay merge.
//
// This test complements TestFulfillStep_WritesGlobalWorkspace, which only
// asserts the AutoDepositResult path (gw["step_1"]). This test asserts the
// overlay merge path (gw["answer"]).
func TestFulfillStep_SingleStepOverlayMerge(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{
			ID:            "step_1",
			RoleID:        "worker",
			Task:          "Compute the answer",
			MergeStrategy: "overlay",
			TargetKey:     "answer",
		},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)

	fulfillment := `{"status":"ok","confidence":0.9,"result":{"answer":42},"capability":"text"}`
	if err := engine.FulfillStep(taskID, dec.ID, fulfillment); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	engine.Mu.RLock()
	graph := engine.graphs[taskID]
	gw := graph.GlobalWorkspace
	engine.Mu.RUnlock()

	// The overlay merge should have written to GlobalWorkspace["answer"]
	// (the TargetKey), not just GlobalWorkspace["step_1"] (the AutoDepositResult).
	overlayVal, ok := gw["answer"]
	if !ok {
		t.Fatal("D3 fast-path regression: GlobalWorkspace[\"answer\"] missing after " +
			"FulfillStep; the single-step fast-path did not copy MergeStrategy/TargetKey " +
			"from StepInput to Step, so the overlay merge was silently skipped")
	}

	// The overlay merge logic (scheduler_decision_success.go:54-55) extracts
	// the specific value under TargetKey from the result, then wraps it:
	// val = map[string]any{TargetKey: specific}
	resultMap, ok := overlayVal.(map[string]any)
	if !ok {
		t.Fatalf("expected GlobalWorkspace[\"answer\"] to be a map, got %T: %v", overlayVal, overlayVal)
	}
	if resultMap["answer"] != float64(42) {
		t.Errorf("expected GlobalWorkspace[\"answer\"][\"answer\"]=42, got %v", resultMap["answer"])
	}
}
