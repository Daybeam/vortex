package schemas

import (
	"reflect"
	"testing"
)

// O59 regression: a host fulfilling a delegated step must be able to inspect the
// same exit criteria and deterministic checks declared on that step.
func TestToStatusDictView_FullDisclosesStepCriteria(t *testing.T) {
	checks := []DeterministicCheck{{Type: "file_exists", Params: map[string]any{"path": "report.md"}}}
	g := &TaskGraph{
		TaskID: "t1", Status: GraphBlocked,
		Steps: map[string]*Step{
			"s1": {ID: "s1", Status: StepBlocked, ExitCriteria: "file:report.md", DeterministicChecks: checks},
		},
	}
	out := g.ToStatusDictView("full")
	step := out["steps"].(map[string]any)["s1"].(map[string]any)
	if step["exit_criteria"] != "file:report.md" {
		t.Fatalf("exit_criteria = %v", step["exit_criteria"])
	}
	if !reflect.DeepEqual(step["deterministic_checks"], checks) {
		t.Fatalf("deterministic_checks = %#v, want %#v", step["deterministic_checks"], checks)
	}
}
