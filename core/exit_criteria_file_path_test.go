package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// O57: a relative `file:` exit criterion was statted against the engine process
// cwd, while the delegation envelope tells the host that relative paths mean
// task_output_dir (core/spawner_delegation_workspace.go:16,19).
//
// Before the fix, `verifyExitCriteria` did `os.Stat(path)` on the raw criterion
// string. A host that followed the advertised contract — writing report.md into
// task_output_dir via orchestrator_run_command with cwd = task_output_dir —
// always failed its own gate, and only a criterion spelled as an absolute path
// could ever pass. That made the "use the engine's tool surface" tier of O54
// recommend something it could not deliver.
//
// Three conventions existed for the same promise; the register records them:
// envelope = outputBase/<taskID>, deterministic_checks workdir = outputBase
// (core/scheduler_decision_audit.go:283 + core/verify/deterministic_verifier.go:41),
// `file:` = engine process cwd (unjoined).

func TestExitCriteriaFileCriterionResolvesTaskRelative(t *testing.T) {
	engine := newDelegationTestEngine(t)
	const taskID = "t_o57_filecriterion"
	const criterion = "zz_o57_report.md"

	taskDir := filepath.Join(engine.outputBase, taskID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir task dir: %v", err)
	}

	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{},
	}
	step := &schemas.Step{ID: "s1", RoleID: "worker", ExitCriteria: "file:" + criterion}
	graph.Steps["s1"] = step

	result := &SpawnResult{Output: schemas.SubagentOutput{
		Status:     schemas.StatusOK,
		Confidence: 0.9,
		Result:     map[string]any{"answer": "written"},
	}}

	// Negative control first: with no file anywhere, the gate must fail, and it
	// must say it looked inside task_output_dir — that is the contract the
	// envelope advertises. If the reason still names the process cwd, O57 is
	// back.
	ok, reason, _ := engine.verifyExitCriteria(context.Background(), graph, step, result)
	if ok {
		t.Fatalf("gate passed with no file present (reason=%q)", reason)
	}
	if !strings.Contains(filepath.ToSlash(reason), filepath.ToSlash(taskDir)) {
		t.Fatalf("O57 regression: relative file: criterion resolved outside the task dir; got %q, want a path under %q",
			reason, taskDir)
	}

	// A task-relative write, exactly what the envelope asks the host for. The
	// body must clear the gate's 1 KiB floor.
	if err := os.WriteFile(filepath.Join(taskDir, criterion), []byte(strings.Repeat("x", 2048)), 0o644); err != nil {
		t.Fatalf("write task file: %v", err)
	}
	if ok, reason, _ := engine.verifyExitCriteria(context.Background(), graph, step, result); !ok {
		t.Fatalf("task-relative file: criterion still fails after O57 fix: %q", reason)
	}

	// Absolute criteria must keep working — the fix only joins when the path is
	// relative.
	absStep := &schemas.Step{ID: "s2", RoleID: "worker", ExitCriteria: "file:" + filepath.Join(taskDir, criterion)}
	graph.Steps["s2"] = absStep
	if ok, reason, _ := engine.verifyExitCriteria(context.Background(), graph, absStep, result); !ok {
		t.Fatalf("absolute file: criterion regressed: %q", reason)
	}
}
