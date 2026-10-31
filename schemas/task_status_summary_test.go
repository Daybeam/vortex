package schemas

import "testing"

// TestToStatusDictView_SummaryReportsPendingDecisionCount is the regression
// test for Issue-1 (2026-10-23). A task that is blocked awaiting a decision
// keeps task-level status "running" — see core/scheduler_decision_gate.go:
// "We do NOT set graph.Status = GraphBlocked here". A caller reading only the
// summary view therefore cannot distinguish "still working" from "waiting for
// me" and may misreport a stalled task as live. pending_decision_count is the
// disambiguator and must be present in the summary.
func TestToStatusDictView_SummaryReportsPendingDecisionCount(t *testing.T) {
	g := &TaskGraph{
		TaskID: "t1",
		Status: GraphRunning,
		// A step is blocked awaiting a decision; the task is still "running".
		Steps: map[string]*Step{"s1": {ID: "s1", Status: StepBlocked}},
	}

	out := g.ToStatusDictView("summary")
	got, present := out["pending_decision_count"]
	if !present {
		t.Fatal("summary view must carry pending_decision_count")
	}
	if got != 0 {
		t.Fatalf("expected pending_decision_count=0, got %v", got)
	}
	if out["status"] != string(GraphRunning) {
		t.Fatalf("status must stay running while awaiting a decision, got %v", out["status"])
	}

	g.PendingDecisions = append(g.PendingDecisions, &Decision{ID: "dec1"})
	out = g.ToStatusDictView("summary")
	if got := out["pending_decision_count"]; got != 1 {
		t.Fatalf("expected pending_decision_count=1 after enqueue, got %v", got)
	}
}

// TestToStatusDictView_FullUnaffectedBySummaryField guards the additive nature
// of the fix: the "full" view must not change shape because of the new summary
// field (it is not populated there).
func TestToStatusDictView_FullUnaffectedBySummaryField(t *testing.T) {
	g := &TaskGraph{
		TaskID: "t1",
		Status: GraphRunning,
		Steps:  map[string]*Step{"s1": {ID: "s1", Status: StepRunning}},
	}
	out := g.ToStatusDictView("full")
	if _, present := out["pending_decision_count"]; present {
		t.Fatal("pending_decision_count belongs to the summary view only")
	}
}