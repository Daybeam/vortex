package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// The replay canvas redraws step dependency edges from the task_submitted log
// payload. Keep that contract stable: only steps with upstream deps are logged,
// and fully parallel plans omit the key entirely.
func TestTaskSubmittedDetailIncludesDeps(t *testing.T) {
	graph := &schemas.TaskGraph{Steps: map[string]*schemas.Step{
		"s1": {ID: "s1"},
		"s2": {ID: "s2", DependsOn: []string{"s1"}},
	}}
	detail := taskSubmittedDetail(2, false, graph)
	if detail["step_count"] != 2 {
		t.Fatalf("step_count = %v, want 2", detail["step_count"])
	}
	deps, ok := detail["deps"].(map[string][]string)
	if !ok {
		t.Fatalf("deps = %T, want map[string][]string", detail["deps"])
	}
	if len(deps) != 1 || len(deps["s2"]) != 1 || deps["s2"][0] != "s1" {
		t.Fatalf("deps = %+v, want only s2 -> s1", deps)
	}
	if _, exists := deps["s1"]; exists {
		t.Errorf("s1 has no upstream deps and must not be logged")
	}

	flat := taskSubmittedDetail(1, false, &schemas.TaskGraph{Steps: map[string]*schemas.Step{
		"s1": {ID: "s1"},
	}})
	if _, exists := flat["deps"]; exists {
		t.Errorf("parallel-only graph must omit the deps key, got %v", flat["deps"])
	}
}
