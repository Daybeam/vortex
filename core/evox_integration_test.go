package core

import (
	"context"
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestEvoXCoordination(t *testing.T) {
	// Setup a minimal registry and store
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"tester": {
				ID:             "tester",
				BaseCapability: "test",
				Provider:       "mock",
			},
		},
		Providers: make(map[string]*config.ProviderConfig),
	}

	ts := store.NewTaskStore(store.NewFileTaskBackend("test_tasks_evox"))
	defer os.RemoveAll("test_tasks_evox")

	sys := &config.SystemSettings{}
	es, _ := store.NewExperienceStore("test_exp_evox", ts, sys, nil, nil)
	defer os.RemoveAll("test_exp_evox")

	logger, _ := NewLogger(t.TempDir(), sys)
	// FIX (playbook addendum): without this, the async Logger writer
	// goroutine can still be draining a queued write when t.TempDir()'s
	// registered cleanup runs, racing to delete a directory Windows still
	// considers non-empty ("the directory is not empty"). Logger.Close()
	// correctly waits for the writer to drain before returning.
	defer logger.Close()
	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, "test_out_evox", "test_tmp_evox", nil)
	defer os.RemoveAll("test_out_evox")
	defer os.RemoveAll("test_tmp_evox")

	// 1. Test Programmatic Merge
	graph := &schemas.TaskGraph{
		TaskID:          "test_evox_1",
		GlobalWorkspace: make(map[string]any),
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:            "s1",
				MergeStrategy: "append",
				TargetKey:     "items",
				Status:        schemas.StepRunning,
			},
			"s2": {
				ID:            "s2",
				MergeStrategy: "append",
				TargetKey:     "items",
				Status:        schemas.StepRunning,
			},
		},
	}

	res1 := &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusOK,
			Confidence: 1.0,
			Result:     map[string]any{"items": []string{"a", "b"}},
		},
	}
	res2 := &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusOK,
			Confidence: 1.0,
			Result:     map[string]any{"items": []string{"c", "d"}},
		},
	}

	engine.handleOutput(graph, graph.Steps["s1"], res1)
	engine.handleOutput(graph, graph.Steps["s2"], res2)

	merged := graph.GlobalWorkspace["items"].([]any)
	if len(merged) != 2 {
		t.Errorf("Expected 2 merged items (blocks), got %d", len(merged))
	}

	t.Logf("Merged Workspace: %+v", graph.GlobalWorkspace)

	// 2. Test Input Mapping Resolution
	step3 := &schemas.Step{
		ID: "s3",
		InputMapping: map[string]string{
			"pipe_data": "items",
		},
		Status: schemas.StepPending,
	}
	graph.Steps["s3"] = step3

	// Simulate executeStep's resolution logic
	resolved := make(map[string]any)
	for arg, path := range step3.InputMapping {
		if val, ok := graph.GlobalWorkspace[path]; ok {
			resolved[arg] = val
		}
	}

	if resolved["pipe_data"] == nil {
		t.Error("InputMapping failed to resolve 'items' from GlobalWorkspace")
	} else {
		list := resolved["pipe_data"].([]any)
		if len(list) != 2 {
			t.Errorf("Expected mapped list size 2, got %d", len(list))
		}
	}

	// 3. Test Role Affinity (EvoX)
	// Reflection should update affinity
	records := []store.StepRecord{
		{
			StepID:     "s1",
			RoleID:     "tester",
			Status:     "ok",
			Capability: "test",
		},
	}
	// Simulate reflection
	engine.expStore.RecordTaskCompletion(context.Background(), "test_task", records, 1.0, nil, true, false)

	affinity := engine.expStore.QueryRoleAffinity(context.Background(), "tester", "test")
	if affinity <= 0.5 {
		t.Errorf("Expected affinity to increase (>0.5), got %f", affinity)
	}
	t.Logf("Tester Affinity for 'test': %f", affinity)
}
