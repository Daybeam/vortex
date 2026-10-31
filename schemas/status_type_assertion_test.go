package schemas

import (
	"testing"
	"time"
)

// TestStatusDictView_StatusFieldIsString is a regression test for F4
// (T2_VERIFICATION_AND_DRIFT_FOLLOWUP.md §2.3 issue 1).
//
// Bug: ToStatusDictView stored GraphStatus (a named string type) directly
// into map[string]any. Callers doing result["status"].(string) got "" and
// false, because Go type assertions require identical dynamic types —
// GraphStatus != string even though type GraphStatus string.
//
// Fix: convert to string() at the source in ToStatusDictView.
//
// This test verifies the .(string) assertion succeeds for:
//   - graph-level "status" (GraphStatus) in both summary and full views
//   - step-level "status" (StepStatus) in the full view
//   - context-tree node "status" (NodeStatus) in the full view
func TestStatusDictView_StatusFieldIsString(t *testing.T) {
	graph := &TaskGraph{
		TaskID:  "task-f4-regression",
		TraceID: "trace-f4",
		Status:  GraphRunning,
		Steps: map[string]*Step{
			"s1": {
				ID:     "s1",
				Status: StepRunning,
				Task:   "do something",
			},
		},
		PendingDecisions: []*Decision{},
		OutputFiles:      []OutputFile{},
		CreatedAt:        time.Now(),
		ContextTree: map[string]*ContextNode{
			"n1": {
				ID:     "n1",
				Status: NodeActive,
				Intent: "test intent",
			},
		},
		GlobalWorkspace: map[string]any{},
		SessionRoles:    map[string]any{},
		SessionSkills:   map[string]any{},
	}

	// --- Summary view: graph-level status ---
	summary := graph.ToStatusDictView("summary")
	s, ok := summary["status"].(string)
	if !ok {
		t.Fatalf("summary[\"status\"].(string) failed: dynamic type is %T, expected string. "+
			"F4 regression: GraphStatus named type must be converted to string in ToStatusDictView.",
			summary["status"])
	}
	if s != "running" {
		t.Errorf("summary[\"status\"] = %q, want \"running\"", s)
	}

	// --- Full view: graph-level status ---
	full := graph.ToStatusDictView("full")
	gs, ok := full["status"].(string)
	if !ok {
		t.Fatalf("full[\"status\"].(string) failed: dynamic type is %T, expected string. "+
			"F4 regression: GraphStatus named type must be converted to string in ToStatusDictView.",
			full["status"])
	}
	if gs != "running" {
		t.Errorf("full[\"status\"] = %q, want \"running\"", gs)
	}

	// --- Full view: step-level status ---
	steps, ok := full["steps"].(map[string]any)
	if !ok {
		t.Fatal("full[\"steps\"] is not map[string]any")
	}
	stepEntry, ok := steps["s1"].(map[string]any)
	if !ok {
		t.Fatal("steps[\"s1\"] is not map[string]any")
	}
	ss, ok := stepEntry["status"].(string)
	if !ok {
		t.Fatalf("step[\"status\"].(string) failed: dynamic type is %T, expected string. "+
			"F4 regression: StepStatus named type must be converted to string in ToStatusDictView.",
			stepEntry["status"])
	}
	if ss != "running" {
		t.Errorf("step[\"status\"] = %q, want \"running\"", ss)
	}

	// --- Full view: context-tree node status ---
	tree, ok := full["context_tree"].(map[string]map[string]any)
	if !ok {
		t.Fatal("full[\"context_tree\"] is not map[string]map[string]any")
	}
	nodeEntry, ok := tree["n1"]
	if !ok {
		t.Fatal("context_tree[\"n1\"] not found")
	}
	ns, ok := nodeEntry["status"].(string)
	if !ok {
		t.Fatalf("node[\"status\"].(string) failed: dynamic type is %T, expected string. "+
			"F4 regression: NodeStatus named type must be converted to string in ToStatusDictView.",
			nodeEntry["status"])
	}
	if ns != string(NodeActive) {
		t.Errorf("node[\"status\"] = %q, want %q", ns, string(NodeActive))
	}
}
