package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// Regression for docs/plan/O47_O35_Q3_ADJUSTMENT_PLAN.md §3.5: the ACP
// terminal event can omit the answer even though the successful step result
// is persisted. ResolveTaskResult must recover that answer for ACP clients.
func TestResolveTaskResult_FromPersistedStep(t *testing.T) {
	ctx := context.Background()
	taskStore := store.NewTaskStore(store.NewFileTaskBackend(t.TempDir()))
	if err := taskStore.Set(ctx, "task-1", "step-1", &store.StepResult{
		Data: map[string]any{"raw": "persisted final answer"},
	}); err != nil {
		t.Fatalf("seed step result: %v", err)
	}

	engine := &DirectedEngine{
		taskStore:    taskStore,
		lifecycleCtx: ctx,
		graphs: map[string]*schemas.TaskGraph{
			"task-1": {
				TaskID: "task-1",
				Steps: map[string]*schemas.Step{
					"step-1": {ID: "step-1", Status: schemas.StepOK},
				},
			},
		},
	}

	if got := engine.ResolveTaskResult("task-1"); got != "persisted final answer" {
		t.Fatalf("ResolveTaskResult() = %q, want persisted final answer", got)
	}
}

func TestResolveTaskResult_PrefersTerminalStep(t *testing.T) {
	ctx := context.Background()
	taskStore := store.NewTaskStore(store.NewFileTaskBackend(t.TempDir()))
	for stepID, answer := range map[string]string{
		"step-1": "upstream answer",
		"step-2": "terminal answer",
	} {
		if err := taskStore.Set(ctx, "task-1", stepID, &store.StepResult{
			Data: map[string]any{"text": answer},
		}); err != nil {
			t.Fatalf("seed %s: %v", stepID, err)
		}
	}

	engine := &DirectedEngine{
		taskStore:    taskStore,
		lifecycleCtx: ctx,
		graphs: map[string]*schemas.TaskGraph{
			"task-1": {
				TaskID: "task-1",
				Steps: map[string]*schemas.Step{
					"step-1": {ID: "step-1", Status: schemas.StepOK},
					"step-2": {ID: "step-2", Status: schemas.StepOK, DependsOn: []string{"step-1"}},
				},
			},
		},
	}

	if got := engine.ResolveTaskResult("task-1"); got != "terminal answer" {
		t.Fatalf("ResolveTaskResult() = %q, want terminal answer", got)
	}
}

func TestResolveTaskResult_TruncatesLargeAnswer(t *testing.T) {
	ctx := context.Background()
	taskStore := store.NewTaskStore(store.NewFileTaskBackend(t.TempDir()))
	answer := strings.Repeat("x", 10050)
	if err := taskStore.Set(ctx, "task-1", "step-1", &store.StepResult{Data: answer}); err != nil {
		t.Fatalf("seed step result: %v", err)
	}

	engine := &DirectedEngine{
		taskStore:    taskStore,
		lifecycleCtx: ctx,
		graphs: map[string]*schemas.TaskGraph{
			"task-1": {
				TaskID: "task-1",
				Steps: map[string]*schemas.Step{
					"step-1": {ID: "step-1", Status: schemas.StepOK},
				},
			},
		},
	}

	got := engine.ResolveTaskResult("task-1")
	if len(got) != 10014 || !strings.HasSuffix(got, "...[truncated]") {
		t.Fatalf("ResolveTaskResult() length/suffix = %d/%q, want truncated result", len(got), got)
	}
}
