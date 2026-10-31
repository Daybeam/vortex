package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// scheduler_delegation_next_batch_test.go — §4.2 regression test
// (DELEGATION_HANDOVER_REDESIGN.md §4.2, submit_decision returns next batch).
//
// Bug: submit_decision response only had {ok, human_approval}. The host had
// to poll wait_task after every fulfill to get the next delegation envelope.
// Fix: after FulfillStep succeeds, call FirstDelegations(500ms) and include
// the next batch in the response, reducing N-step round-trips from 1+2N to N+1.

// TestFulfillStep_NextBatchDelegations verifies that after fulfilling step_a
// in a 2-step DAG, FirstDelegations returns the next delegation (for step_b).
// This is the engine behavior that the §4.2 handler fix relies on.
func TestFulfillStep_NextBatchDelegations(t *testing.T) {
	engine := newDelegationTestEngine(t)

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "step_a", RoleID: "worker", Task: "First step"},
		{ID: "step_b", RoleID: "worker", Task: "Second step", DependsOn: []string{"step_a"}},
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	// Wait for the first delegation decision (for step_a).
	decA := waitForDelegationDecision(t, engine, taskID)

	// Fulfill step_a.
	fulfillment := `{"status":"ok","confidence":0.9,"result":{"answer":42},"capability":"text"}`
	if err := engine.FulfillStep(taskID, decA.ID, fulfillment); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	// §4.2: after fulfilling step_a, FirstDelegations should return the
	// next delegation (for step_b) within a short timeout.
	delegations, found := engine.FirstDelegations(context.Background(), taskID, 3*time.Second)
	if !found {
		t.Fatalf("expected next batch of delegations after fulfilling step_a, got none")
	}
	if len(delegations) != 1 {
		t.Fatalf("expected 1 delegation for step_b, got %d", len(delegations))
	}
	if delegations[0].StepID != "step_b" {
		t.Fatalf("expected delegation for step_b, got step_id=%s", delegations[0].StepID)
	}
}
