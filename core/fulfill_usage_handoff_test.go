package core

import (
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// O56: FulfillStep parses the host's payload into a full SubagentOutput
// (core/scheduler_status.go:426) but handed handleStepSuccess a literal that
// omitted Usage. costTracker.Record is guarded by output.Usage != nil
// (core/scheduler_decision_success.go:38) and the EventStepCompleted usage
// payload by the same check (:28), so a host that reported its token spend was
// recorded nowhere at all — the step looked free.
//
// This test pins the half that needed no decision: a field the host already
// sends must survive the handoff. Advancing graph.TokensUsed from that number is
// deliberately not asserted here — it makes DecisionBudgetExhausted reachable on
// the fulfill path, whose only implemented choice is abort
// (core/scheduler_dag_exec.go:58-70 vs core/scheduler_decision_answer.go:157-341). That
// stays an open decision in register O56.

func TestFulfillStep_UsageReachesCostTracker(t *testing.T) {
	engine := newDelegationTestEngine(t)

	ct, err := NewCostTracker(filepath.Join(t.TempDir(), "cost.db"))
	if err != nil {
		t.Fatalf("NewCostTracker: %v", err)
	}
	defer ct.Close()
	engine.SetCostTracker(ct)

	taskID, err := engine.Submit([]schemas.StepInput{{
		ID:     "step_1",
		RoleID: "worker",
		Task:   "Reply with the single word HELLO.",
	}})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)
	payload := `{"status":"ok","confidence":0.9,` +
		`"result":{"answer":"HELLO"},"capability":"text",` +
		`"usage":{"prompt_tokens":1200,"completion_tokens":340,"total_tokens":1540}}`
	if err := engine.FulfillStep(taskID, dec.ID, payload); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	// handleStepSuccess runs synchronously before FulfillStep returns, so the
	// row is already committed here.
	spend, err := ct.GetTotalSpend()
	if err != nil {
		t.Fatalf("GetTotalSpend: %v", err)
	}
	total := 0
	for _, tokens := range spend {
		total += tokens
	}
	if total != 1540 {
		t.Fatalf("O56 still open: host-reported usage never reached the cost ledger, total=%d by model=%v (want 1540)", total, spend)
	}
}
