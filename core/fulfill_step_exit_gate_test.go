package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// O52: a step's exit_criteria is never enforced when the step is answered by a
// delegation fulfill.
//
// verifyExitCriteria is the only door to all four quality gates (artifact
// contract, regex/file, deterministic checks, VerifierModel audit) and it has
// exactly two call sites, both on the spawn path:
// core/scheduler_dag_exec.go:670 and core/scheduler_submit.go:508. FulfillStep
// (core/scheduler_status.go:363) sets step.Status = StepOK and calls
// handleStepSuccess directly, so a host-answered step leaves that door shut.
//
// Measured live 2026-11-06 (tmp/o52-fulfill-exitgate/, fresh binary,
// VORTEX_DELEGATION_MODE=true, one step, exit_criteria
// "regex:ZZZ_THIS_TOKEN_MUST_NEVER_APPEAR_ZZZ"): the task reached `completed`,
// steps.s1 = `ok`, last_error = "", and all 21 events for the task contain zero
// exit_criteria_fail / step_retrying. That run proves the symptom; this test
// proves the mechanism and keeps it provable without a provider, because the
// regex branch short-circuits before any model call.
//
// O53 keeps this boundary: the delegate may execute in another environment,
// so engine-side gates must not run on fulfill. Payload-local mismatches are
// warnings only (fulfill_payload_warning_test.go), never retries or rejection.

func TestFulfillStep_ExitCriteriaGateNeverRuns(t *testing.T) {
	engine := newDelegationTestEngine(t)

	const criterion = "regex:ZZZ_THIS_TOKEN_MUST_NEVER_APPEAR_ZZZ"
	taskID, err := engine.Submit([]schemas.StepInput{{
		ID:           "step_1",
		RoleID:       "worker",
		Task:         "Reply with the single word HELLO.",
		ExitCriteria: criterion,
	}})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}

	dec := waitForDelegationDecision(t, engine, taskID)
	fulfillment := `{"status":"ok","confidence":0.95,"result":{"answer":"HELLO"},"capability":"text"}`
	if err := engine.FulfillStep(taskID, dec.ID, fulfillment); err != nil {
		t.Fatalf("FulfillStep failed: %v", err)
	}

	graph := engine.GetTaskGraph(taskID)
	step := graph.Steps["step_1"]
	status, lastErr := step.Status, step.LastError
	refineCount := step.AutoRefineCount

	// Positive control: the very same (step, payload) pair does fail the gate,
	// and fails it for this criterion — not for some unrelated reason. Without
	// this, "no exit_criteria_fail event" would be vacuous.
	ok, reason, _ := engine.verifyExitCriteria(context.Background(), graph, step, &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusOK,
			Confidence: 0.95,
			Result:     map[string]any{"answer": "HELLO"},
		},
	})
	if ok {
		t.Fatal("verifyExitCriteria accepted an output that does not match the step's regex criterion; " +
			"the control is broken and the rest of this test proves nothing")
	}
	pattern := strings.TrimPrefix(criterion, "regex:")
	if !strings.Contains(reason, pattern) {
		t.Fatalf("gate failed for the wrong reason: %q (want the criterion %q)", reason, pattern)
	}

	// The finding: despite the above, the fulfilled step sailed through. The
	// spawn path would have moved it back to StepPending with an
	// "Exit criteria not met" last_error and spent an auto-refine.
	if status != schemas.StepOK {
		t.Fatalf("O52 no longer reproduces: step status=%s after fulfill (was StepOK)", status)
	}
	if lastErr != "" {
		t.Fatalf("O52 no longer reproduces: fulfill left last_error=%q", lastErr)
	}
	if refineCount != 0 {
		t.Fatalf("O52 no longer reproduces: auto_refine_count=%d after fulfill", refineCount)
	}
}
