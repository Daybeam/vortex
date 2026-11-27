package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// O58: structured checks must use the task output directory, not the shared
// outputs root, and public-tier submissions must not execute command checks.
func TestVerifyExitCriteria_DeterministicChecksUseTaskDir(t *testing.T) {
	engine := newDelegationTestEngine(t)
	const taskID = "t_o58_taskdir"
	taskDir := filepath.Join(engine.outputBase, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "report.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	graph := &schemas.TaskGraph{TaskID: taskID, SubmitterIsAdmin: true, Steps: map[string]*schemas.Step{}}
	step := &schemas.Step{ID: "s1", DeterministicChecks: []schemas.DeterministicCheck{{
		Type: "file_exists", Params: map[string]any{"path": "report.json"},
	}}}
	graph.Steps[step.ID] = step
	ok, reason, _ := engine.verifyExitCriteria(context.Background(), graph, step, &SpawnResult{})
	if !ok {
		t.Fatalf("task-relative check failed: %s", reason)
	}
}

func TestVerifyExitCriteria_PublicCannotRunCommandCheck(t *testing.T) {
	engine := newDelegationTestEngine(t)
	graph := &schemas.TaskGraph{TaskID: "t_o58_public", SubmitterIsAdmin: false, Steps: map[string]*schemas.Step{}}
	step := &schemas.Step{ID: "s1", DeterministicChecks: []schemas.DeterministicCheck{{
		Type: "command_pass", Params: map[string]any{"command": "echo should-not-run"},
	}}}
	graph.Steps[step.ID] = step
	ok, reason, failType := engine.verifyExitCriteria(context.Background(), graph, step, &SpawnResult{})
	if ok || failType != schemas.FailurePermission {
		t.Fatalf("public command check was not denied: ok=%v reason=%q failType=%q", ok, reason, failType)
	}
}
