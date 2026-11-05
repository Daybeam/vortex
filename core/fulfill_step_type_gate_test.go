package core

import (
	"strings"
	"testing"
)

// Regression test for O17 (docs/audit/unwired-code-register.md §O17).
//
// orchestrator_submit_decision routes on payload presence, not on decision type:
// shouldRouteToFulfill (tools/tools_task.go:66-67) sends any call with a non-empty
// `output` to FulfillStep, which is delegation-only. A host that answers a
// step_failed decision with choice=abort and an empty JSON object therefore lands
// here, and the old message — `decision "dec_x" is not a delegation` — named a
// concept the caller had never mentioned. Reading only that error I recorded the
// false finding "a step_failed decision can be parked but never answered"; the
// same decision answered payload-free returned {"ok":true}.
//
// The message is the fix's only observable surface, so it is what this pins: it
// must name the decision's real type and the payload rule that actually applies.

func TestFulfillStep_MisRoutedAnswerNamesTheRealDecisionType(t *testing.T) {
	s, _ := newAnswerTestEngine(t)
	taskID := "o17_misrouted_abort"
	s.graphs[taskID] = newBlockedGraph(taskID) // step_failed decision, options skip|abort
	s.doneChans[taskID] = make(chan struct{})

	err := s.FulfillStep(taskID, "dec_o25", "{}")
	if err == nil {
		t.Fatal("FulfillStep accepted a step_failed decision: the type gate is gone")
	}
	msg := err.Error()
	if !strings.Contains(msg, "step_failed") {
		t.Fatalf("error does not name the decision's real type (O17): %q", msg)
	}
	if !strings.Contains(msg, "output payload") {
		t.Fatalf("error does not state that skip|abort carry no payload (O17): %q", msg)
	}

	// The payload-free answer the caller should have sent still works, and must
	// keep working: this is the half of O17 that corrected my own first reading.
	if err := s.SubmitDecisionWithPayload(taskID, "dec_o25", "abort", ""); err != nil {
		t.Fatalf("payload-free abort rejected: %v", err)
	}
}
