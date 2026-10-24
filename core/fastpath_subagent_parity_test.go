package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestFastPath_PartialIsNotSuccess is the regression test for the subagent
// standard (2026-10-23). A single-step submission runs the inline fast path
// (scheduler_submit.go). Before the fix, that path collapsed EVERY
// non-delegation subagent status to StepOK + GraphCompleted, so a subagent
// reporting `partial` was reported to the caller as success. Now partial must
// surface as StepPartial and raise a decision (the caller verifies).
func TestFastPath_PartialIsNotSuccess(t *testing.T) {
	engine := newLifecycleEngine(t)
	// Turn 0 does real work; turn 1 declares a PARSEABLE partial (conf 0.9).
	// A prior tool call is required so the turn is not force-tool-nudged.
	p := &lifecycleProvider{script: []*schemas.ProviderResponse{
		lifecycleToolCall("partial_work.txt"),
		lifecycleFinal("partial"),
	}}
	wireLifecycleProvider(t, engine, p)

	taskID, err := engine.Submit([]schemas.StepInput{{ID: "s1", RoleID: "worker", Task: "do the thing"}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// WaitTask returns as soon as a decision is pending.
	out, ok := engine.WaitTask(ctx, taskID, 10*time.Second, "full")
	if !ok {
		t.Fatal("task not found")
	}

	steps, _ := out["steps"].(map[string]any)
	if steps == nil {
		t.Fatalf("full view has no steps: %#v", out)
	}
	s1, _ := steps["s1"].(map[string]any)
	if s1 == nil {
		t.Fatalf("step s1 missing: %#v", steps)
	}
	if s1["status"] != schemas.StepPartial {
		t.Fatalf("fast-path partial must surface as StepPartial, got %v", s1["status"])
	}
	if out["status"] == schemas.GraphCompleted {
		t.Fatalf("a partial subagent result must not complete the task (status=%v)", out["status"])
	}

	decs, _ := out["pending_decisions"].([]map[string]any)
	if len(decs) == 0 {
		t.Fatalf("partial must raise a verification decision, got %#v", out["pending_decisions"])
	}
}

// TestFastPath_FailedIsNotSuccess pins the sibling case: a subagent reporting
// `failed` must terminate the task as failed, never as completed.
func TestFastPath_FailedIsNotSuccess(t *testing.T) {
	engine := newLifecycleEngine(t)
	p := &lifecycleProvider{script: []*schemas.ProviderResponse{
		lifecycleToolCall("work.txt"),
		lifecycleFinal("failed"),
	}}
	wireLifecycleProvider(t, engine, p)

	taskID, err := engine.Submit([]schemas.StepInput{{ID: "s1", RoleID: "worker", Task: "do the thing"}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, ok := engine.WaitTask(ctx, taskID, 10*time.Second, "full")
	if !ok {
		t.Fatal("task not found")
	}
	if out["status"] == schemas.GraphCompleted {
		t.Fatalf("a failed subagent result must not complete the task, got %v", out["status"])
	}
	steps, _ := out["steps"].(map[string]any)
	s1, _ := steps["s1"].(map[string]any)
	if s1 == nil {
		t.Fatalf("step s1 missing: %#v", steps)
	}
	if s1["status"] != schemas.StepFailed {
		t.Fatalf("fast-path failed must surface as StepFailed, got %v", s1["status"])
	}
}

// TestAddDecision_InjectsTaskProductsForDecider is the regression test for the
// decider global-view supplement (2026-10-23): the decision context must carry
// the products in the task's space (result_ref, output_files, artifacts,
// workspace_keys) so the decider can see what was produced before choosing.
func TestAddDecision_InjectsTaskProductsForDecider(t *testing.T) {
	engine := newTestEngineForHandleOutput(t, nil)

	graph := &schemas.TaskGraph{
		TaskID:           "task_decider_view",
		Steps:            map[string]*schemas.Step{},
		PendingDecisions: []*schemas.Decision{},
		OutputFiles: []schemas.OutputFile{
			{StepID: "s0", Path: "/out/report.md", Format: "md", Summary: "report"},
		},
		Artifacts: []schemas.ArtifactContract{
			{Path: "/out/data.json", Status: "validated"},
		},
		GlobalWorkspace: map[string]any{"summary": "done", "items": []any{1, 2}},
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepPartial, ResultRef: "task_decider_view:s1"}
	graph.Steps["s1"] = step

	engine.addDecision(graph, step, schemas.DecisionStepFailed,
		map[string]any{"root_cause": "partial"}, []string{"skip", "abort"})

	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(graph.PendingDecisions))
	}
	ctx := graph.PendingDecisions[0].Context

	if ctx["result_ref"] != "task_decider_view:s1" {
		t.Errorf("result_ref = %v, want the step ref", ctx["result_ref"])
	}
	of, ok := ctx["output_files"].([]schemas.OutputFile)
	if !ok || len(of) != 1 || of[0].Path != "/out/report.md" {
		t.Errorf("output_files = %#v, want one /out/report.md", ctx["output_files"])
	}
	arts, ok := ctx["artifacts"].([]schemas.ArtifactContract)
	if !ok || len(arts) != 1 || arts[0].Path != "/out/data.json" {
		t.Errorf("artifacts = %#v, want one /out/data.json", ctx["artifacts"])
	}
	keys, ok := ctx["workspace_keys"].([]string)
	if !ok || len(keys) != 2 {
		t.Fatalf("workspace_keys = %#v, want 2 keys", ctx["workspace_keys"])
	}
	if keys[0] != "items" || keys[1] != "summary" {
		t.Errorf("workspace_keys must be sorted, got %v", keys)
	}
}