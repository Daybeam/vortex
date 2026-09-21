package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestAutoDeposit_NilGraph(t *testing.T) {
	// Should not panic
	AutoDepositResult(nil, "s1", map[string]any{"key": "val"})
}

func TestAutoDeposit_EmptyStepID(t *testing.T) {
	graph := &schemas.TaskGraph{TaskID: "t1"}
	AutoDepositResult(graph, "", map[string]any{"key": "val"})
	if graph.GlobalWorkspace != nil {
		t.Fatal("expected nil GlobalWorkspace for empty stepID")
	}
}

func TestAutoDeposit_NilResult(t *testing.T) {
	graph := &schemas.TaskGraph{TaskID: "t1"}
	AutoDepositResult(graph, "s1", nil)
	if graph.GlobalWorkspace != nil {
		t.Fatal("expected nil GlobalWorkspace for nil result")
	}
}

func TestAutoDeposit_InitializesWorkspace(t *testing.T) {
	graph := &schemas.TaskGraph{TaskID: "t1"}
	result := map[string]any{"findings": "book content"}
	AutoDepositResult(graph, "s1", result)

	if graph.GlobalWorkspace == nil {
		t.Fatal("expected non-nil GlobalWorkspace")
	}

	// Should deposit under step ID
	val, ok := graph.GlobalWorkspace["s1"]
	if !ok {
		t.Fatal("expected key 's1' in GlobalWorkspace")
	}
	m, ok := val.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", val)
	}
	if m["findings"] != "book content" {
		t.Fatalf("expected 'book content', got %v", m["findings"])
	}

	// Should also deposit under "result"
	val2, ok := graph.GlobalWorkspace["result"]
	if !ok {
		t.Fatal("expected key 'result' in GlobalWorkspace")
	}
	m2, ok := val2.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any for 'result', got %T", val2)
	}
	if m2["findings"] != "book content" {
		t.Fatalf("expected 'book content' in 'result', got %v", m2["findings"])
	}
}

func TestAutoDeposit_DoesNotOverwriteStepID(t *testing.T) {
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		GlobalWorkspace: map[string]any{
			"s1": map[string]any{"existing": "data"},
		},
	}
	AutoDepositResult(graph, "s1", map[string]any{"new": "data"})

	val := graph.GlobalWorkspace["s1"].(map[string]any)
	if val["existing"] != "data" {
		t.Fatal("expected existing data to be preserved")
	}
	if _, exists := val["new"]; exists {
		t.Fatal("expected new data to NOT overwrite existing")
	}
}

func TestAutoDeposit_DoesNotOverwriteResultKey(t *testing.T) {
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		GlobalWorkspace: map[string]any{
			"result": map[string]any{"existing": "data"},
		},
	}
	AutoDepositResult(graph, "s1", map[string]any{"new": "data"})

	val := graph.GlobalWorkspace["result"].(map[string]any)
	if val["existing"] != "data" {
		t.Fatal("expected existing 'result' to be preserved")
	}
}

func TestAutoDeposit_MultiStepDAG(t *testing.T) {
	// Simulate a 3-step DAG where each step completes sequentially
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1"},
			"s2": {ID: "s2"},
			"s3": {ID: "s3"},
		},
	}

	// s1 completes
	AutoDepositResult(graph, "s1", map[string]any{"summary": "step 1 result"})
	// s2 completes
	AutoDepositResult(graph, "s2", map[string]any{"summary": "step 2 result"})
	// s3 completes
	AutoDepositResult(graph, "s3", map[string]any{"summary": "step 3 result"})

	// Each step should have its own key
	if graph.GlobalWorkspace["s1"] == nil {
		t.Fatal("expected s1 in GlobalWorkspace")
	}
	if graph.GlobalWorkspace["s2"] == nil {
		t.Fatal("expected s2 in GlobalWorkspace")
	}
	if graph.GlobalWorkspace["s3"] == nil {
		t.Fatal("expected s3 in GlobalWorkspace")
	}

	// "result" key should be from the first step that deposited it (s1)
	resultVal := graph.GlobalWorkspace["result"].(map[string]any)
	if resultVal["summary"] != "step 1 result" {
		t.Fatalf("expected 'step 1 result', got %v", resultVal["summary"])
	}
}

func TestAutoDeposit_RespectsExplicitMerge(t *testing.T) {
	// When EvoX explicit merge already set a TargetKey, AutoDeposit should not
	// overwrite it — but it should still deposit under stepID and "result"
	// if those keys don't exist yet.
	graph := &schemas.TaskGraph{
		TaskID: "t1",
		GlobalWorkspace: map[string]any{
			"my_target": map[string]any{"merged": "data"},
		},
	}

	AutoDepositResult(graph, "s1", map[string]any{"raw": "output"})

	// Explicit merge key should be untouched
	if graph.GlobalWorkspace["my_target"].(map[string]any)["merged"] != "data" {
		t.Fatal("expected explicit merge data to be preserved")
	}

	// Auto-deposit should still add stepID and "result"
	if graph.GlobalWorkspace["s1"] == nil {
		t.Fatal("expected s1 to be auto-deposited alongside explicit merge")
	}
	if graph.GlobalWorkspace["result"] == nil {
		t.Fatal("expected 'result' to be auto-deposited alongside explicit merge")
	}
}

func TestAutoDeposit_ResultIsReference(t *testing.T) {
	// The deposited map should be the same reference as the input (no deep copy)
	// so callers can mutate it and see changes reflected (e.g. for incremental updates).
	graph := &schemas.TaskGraph{TaskID: "t1"}
	result := map[string]any{"key": "val"}
	AutoDepositResult(graph, "s1", result)

	deposited := graph.GlobalWorkspace["s1"].(map[string]any)
	if &result != &deposited {
		// In Go, map assignment copies the reference (header), so they should
		// point to the same underlying data. Verify by mutation.
		result["new_key"] = "new_val"
		if deposited["new_key"] != "new_val" {
			t.Fatal("expected deposited map to be the same reference as input")
		}
	}
}

func TestAutoDepositSafe_NoPanicOnNilGraph(t *testing.T) {
	// Should not panic, should not log (logger is nil)
	AutoDepositResultSafe(nil, "s1", map[string]any{"key": "val"}, nil)
}
