package schemas

import (
	"encoding/json"
	"testing"
)

// Register O1, planner half: a model-authored plan could not declare
// deterministic checks even after StepInput grew the field, because PlanStep had
// no counterpart and ParsePlanDAG built its StepInput without it. These tests pin
// the plan -> StepInput hop and the exit_criteria trim that shares the same
// budget inside ParsePlanDAG (frozen function size).

func TestParsePlanDAG_CarriesDeclaredChecks(t *testing.T) {
	raw := `{
	  "steps": [
	    {"id":"s1","role_id":"appworld_agent","task":"Find roommate emails",
	     "allowed_tools":["phone__login"],
	     "exit_criteria":"  ",
	     "deterministic_checks":[
	       {"type":"file_exists","params":{"path":"emails.json"}},
	       {"type":"command_pass","params":{"command":"go vet ./..."}}
	     ]},
	    {"id":"s2","role_id":"appworld_agent","task":"Summarise them",
	     "depends_on":["s1"],"allowed_tools":["phone__search_contacts"],
	     "exit_criteria":"regex:summarised"}
	  ]
	}`

	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}

	got := steps[0].DeterministicChecks
	if len(got) != 2 {
		t.Fatalf("planner-declared checks lost at the plan parse: %#v", got)
	}
	if got[0].Type != "file_exists" || got[0].Params["path"] != "emails.json" {
		t.Errorf("check 0 = %q / %#v, want file_exists emails.json", got[0].Type, got[0].Params)
	}
	if got[1].Type != "command_pass" || got[1].Params["command"] != "go vet ./..." {
		t.Errorf("check 1 = %q / %#v, want command_pass", got[1].Type, got[1].Params)
	}

	// A step that declares nothing must stay nil, so Case 2.5 skips it entirely
	// instead of running an empty check list.
	if steps[1].DeterministicChecks != nil {
		t.Errorf("step without checks must have nil, got %#v", steps[1].DeterministicChecks)
	}

	// The whitespace-only criterion collapses to "" rather than reaching the gate
	// as "  " (which the substring branch would then match against the output).
	if steps[0].ExitCriteria != "" {
		t.Errorf("whitespace-only exit_criteria must trim to empty, got %q", steps[0].ExitCriteria)
	}
	if steps[1].ExitCriteria != "regex:summarised" {
		t.Errorf("exit_criteria = %q, want regex:summarised", steps[1].ExitCriteria)
	}
}

// The copied slice must not alias the parsed PlanStep, so mutating a plan's
// checks later cannot rewrite a step already handed to the engine.
func TestParsePlanDAG_ChecksAreNotAliased(t *testing.T) {
	raw := `{
	  "steps": [
	    {"id":"s1","role_id":"appworld_agent","task":"t","allowed_tools":["phone__login"],
	     "deterministic_checks":[{"type":"plan_checkboxes"}]}
	  ]
	}`
	var dag PlanDAG
	if err := json.Unmarshal([]byte(raw), &dag); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if len(dag.Steps) != 1 {
		t.Fatalf("want 1 plan step, got %d", len(dag.Steps))
	}
	steps, err := ParsePlanDAG(raw, testPlanOptions())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	dag.Steps[0].DeterministicChecks[0].Type = "mutated"
	if len(steps[0].DeterministicChecks) == 0 {
		t.Fatal("step carries no checks, so aliasing cannot be observed")
	}
	if steps[0].DeterministicChecks[0].Type == "mutated" {
		t.Fatal("StepInput checks alias the parsed PlanStep")
	}
}
