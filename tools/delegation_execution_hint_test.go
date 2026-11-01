package tools

import (
	"encoding/json"
	"testing"

	"github.com/daybeam/vortex/core"
)

// Regression for §12.3 P1 execution_hint: the delegation envelope must
// carry an execution_hint telling the host how to execute the delegated step.
// Before this fix, DelegationView had no execution mode field, so the host
// had to guess whether to use local_fs, orchestrator_tool, or none.

func TestExecutionHintForTier_AdminGetsLocalFS(t *testing.T) {
	hint := executionHintForTier(TierAdmin)
	if hint == nil {
		t.Fatal("executionHintForTier(admin) = nil, want non-nil")
	}
	if hint.Mode != "local_fs" {
		t.Fatalf("mode = %q, want %q", hint.Mode, "local_fs")
	}
	if hint.Note == "" {
		t.Error("note should be non-empty for admin tier")
	}
}

func TestExecutionHintForTier_PublicGetsNone(t *testing.T) {
	hint := executionHintForTier(TierPublic)
	if hint == nil {
		t.Fatal("executionHintForTier(public) = nil, want non-nil")
	}
	if hint.Mode != "none" {
		t.Fatalf("mode = %q, want %q (public has no execution path)", hint.Mode, "none")
	}
}

func TestEnrichDelegationViews_SetsHintOnAll(t *testing.T) {
	views := []core.DelegationView{
		{StepID: "s1", DecisionID: "d1"},
		{StepID: "s2", DecisionID: "d2"},
		{StepID: "s3", DecisionID: "d3"},
	}
	enrichDelegationViews(views, TierAdmin)
	for i, v := range views {
		if v.ExecutionHint == nil {
			t.Fatalf("views[%d].ExecutionHint = nil, want non-nil", i)
		}
		if v.ExecutionHint.Mode != "local_fs" {
			t.Fatalf("views[%d].mode = %q, want %q", i, v.ExecutionHint.Mode, "local_fs")
		}
	}
}

func TestDelegationView_ExecutionHintSerializes(t *testing.T) {
	// Verify the JSON envelope includes execution_hint when set and omits it when nil.
	v := core.DelegationView{
		StepID:     "s1",
		DecisionID: "d1",
		Kind:       "fulfill",
	}
	v.ExecutionHint = &core.ExecutionHint{Mode: "local_fs", Note: "use run_command"}

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	hint, ok := m["execution_hint"].(map[string]any)
	if !ok {
		t.Fatal("execution_hint missing from JSON output")
	}
	if hint["mode"] != "local_fs" {
		t.Fatalf("execution_hint.mode = %v, want local_fs", hint["mode"])
	}

	// Nil hint → field omitted (omitempty)
	v.ExecutionHint = nil
	data2, _ := json.Marshal(v)
	var m2 map[string]any
	json.Unmarshal(data2, &m2)
	if _, ok := m2["execution_hint"]; ok {
		t.Error("execution_hint should be omitted when nil (backward compat)")
	}
}
