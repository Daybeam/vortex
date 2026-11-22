package core

import (
	"encoding/json"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestSOPLoadPath_UnmarshalAndValidateDAG exercises the combined load path
// (UnmarshalJSON + validateSOPDAG) on a moderate fan-in/fan-out SOP to
// confirm the two stages work end-to-end. Granular DAG and unmarshal cases
// are covered in sop_manager_test.go and schemas/sop_legacy_test.go.
func TestSOPLoadPath_UnmarshalAndValidateDAG(t *testing.T) {
	sopJSON := `{
		"id": "sample_workflow",
		"version": "1.1",
		"author": "human_expert",
		"mutable_by_agent": false,
		"description": "Representative multi-step SOP",
		"triggers": [{"keywords": ["review"]}],
		"steps": {
			"intake": {"id": "intake", "role": "reviewer", "task": "Collect context"},
			"analyze_a": {"id": "analyze_a", "role": "analyst", "task": "Analyze A", "depends_on": ["intake"]},
			"analyze_b": {"id": "analyze_b", "role": "analyst", "task": "Analyze B", "depends_on": ["intake"]},
			"triage": {"id": "triage", "role": "triager", "task": "Triage findings", "depends_on": ["analyze_a", "analyze_b"]},
			"verify": {"id": "verify", "role": "verifier", "task": "Verify triaged items", "depends_on": ["triage"], "exit_criteria": "json"},
			"report": {"id": "report", "role": "synthesizer", "task": "Synthesize report", "depends_on": ["verify"]}
		}
	}`

	var sop schemas.SOP
	if err := json.Unmarshal([]byte(sopJSON), &sop); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := validateSOPDAG(sop.Steps); err != nil {
		t.Fatalf("validateSOPDAG: %v", err)
	}
	if sop.Version != "1.1" {
		t.Errorf("version = %q, want 1.1", sop.Version)
	}
	if len(sop.Steps) != 6 {
		t.Errorf("steps = %d, want 6", len(sop.Steps))
	}
	if got := len(sop.Steps["triage"].DependsOn); got != 2 {
		t.Errorf("triage deps = %d, want 2", got)
	}
	if sop.Steps["verify"].ExitCriteria != "json" {
		t.Errorf("verify exit_criteria = %q, want json", sop.Steps["verify"].ExitCriteria)
	}
}
