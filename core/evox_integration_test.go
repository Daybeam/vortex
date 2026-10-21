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

	ts := store.NewTaskStore(store.NewFileTaskBackend(t.TempDir()))

	sys := &config.SystemSettings{}
	// ExperienceStore spawns background goroutines (debounced persist, state
	// potential save, cooccurrence save) that write to this directory. It has
	// no Close/Stop method, so t.TempDir() cleanup would race with those
	// goroutines on Windows ("directory is not empty"). Use a hardcoded path
	// with os.RemoveAll (which silently ignores partial cleanup failures).
	expDir := "test_exp_evox"
	es := mustNewExperienceStore(t, expDir, ts, sys, nil, nil)
	defer os.RemoveAll(expDir)

	logger := mustNewLogger(t, t.TempDir(), sys)
	// FIX (playbook addendum): without this, the async Logger writer
	// goroutine can still be draining a queued write when t.TempDir()'s
	// registered cleanup runs, racing to delete a directory Windows still
	// considers non-empty ("the directory is not empty"). Logger.Close()
	// correctly waits for the writer to drain before returning.
	defer logger.Close()
	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, t.TempDir(), t.TempDir(), nil)

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

// TestHandleOutput_NilResult verifies that handleOutput returns early
// without panicking when the result is nil. This is a defensive guard
// (L-N15) against nil-deref if a future caller bypasses Spawn.
func TestHandleOutput_NilResult(t *testing.T) {
	// Minimal engine — handleOutput checks result == nil before accessing
	// any engine fields, so a bare DirectedEngine is sufficient.
	engine := &DirectedEngine{}

	graph := &schemas.TaskGraph{
		TaskID:          "t_nil_result",
		GlobalWorkspace: make(map[string]any),
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepRunning},
		},
	}
	step := graph.Steps["s1"]
	originalStatus := step.Status

	// Should not panic.
	engine.handleOutput(graph, step, nil)

	// Step state should be unchanged (early return before setting Confidence).
	if step.Status != originalStatus {
		t.Errorf("step status changed: got %q, want %q", step.Status, originalStatus)
	}
	if step.Confidence != nil {
		t.Error("step confidence should be nil (unchanged by early return)")
	}
}

// TestHandleOutput_LowConfidenceTriggersFailure verifies that a result
// with StatusOK but confidence below the threshold triggers failure
// handling (handleStepFailure), not success handling.
func TestHandleOutput_LowConfidenceTriggersFailure(t *testing.T) {
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
	// Set high threshold so confidence 0.5 < 1.0 triggers failure path.
	reg.System.ConfidenceThreshold = 1.0

	ts := store.NewTaskStore(store.NewFileTaskBackend(t.TempDir()))
	sys := &config.SystemSettings{ConfidenceThreshold: 1.0}

	expDir := "test_exp_low_conf"
	es := mustNewExperienceStore(t, expDir, ts, sys, nil, nil)
	defer os.RemoveAll(expDir)

	logger := mustNewLogger(t, t.TempDir(), sys)
	defer logger.Close()
	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, t.TempDir(), t.TempDir(), nil)

	graph := &schemas.TaskGraph{
		TaskID:          "test_low_conf",
		GlobalWorkspace: make(map[string]any),
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "tester", Status: schemas.StepRunning},
		},
	}
	step := graph.Steps["s1"]

	result := &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusOK,
			Confidence: 0.5, // Below threshold of 1.0
			Result:     map[string]any{"data": "test"},
		},
	}

	// Should not panic — should route to handleStepFailure.
	engine.handleOutput(graph, step, result)

	// Confidence should be set on the step.
	if step.Confidence == nil || *step.Confidence != 0.5 {
		t.Errorf("expected step confidence 0.5, got %v", step.Confidence)
	}

	// Step should be blocked (handleStepFailure sets StepBlocked when no
	// fallback, no spawn depth, no backtrack).
	if step.Status != schemas.StepBlocked {
		t.Errorf("expected step status %q, got %q", schemas.StepBlocked, step.Status)
	}
}

// TestHandleOutput_StatusFailedTriggersFailure verifies that a result
// with StatusFailed routes to handleStepFailure regardless of confidence.
func TestHandleOutput_StatusFailedTriggersFailure(t *testing.T) {
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

	ts := store.NewTaskStore(store.NewFileTaskBackend(t.TempDir()))
	sys := &config.SystemSettings{}

	expDir := "test_exp_status_failed"
	es := mustNewExperienceStore(t, expDir, ts, sys, nil, nil)
	defer os.RemoveAll(expDir)

	logger := mustNewLogger(t, t.TempDir(), sys)
	defer logger.Close()
	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, t.TempDir(), t.TempDir(), nil)

	graph := &schemas.TaskGraph{
		TaskID:          "test_status_failed",
		GlobalWorkspace: make(map[string]any),
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "tester", Status: schemas.StepRunning},
		},
	}
	step := graph.Steps["s1"]

	result := &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusFailed,
			Confidence: 0.95, // High confidence, but status is Failed
			Result:     map[string]any{"error": "execution failed"},
		},
	}

	// Should not panic — should route to handleStepFailure.
	engine.handleOutput(graph, step, result)

	// Confidence should be set.
	if step.Confidence == nil || *step.Confidence != 0.95 {
		t.Errorf("expected step confidence 0.95, got %v", step.Confidence)
	}

	// Step should be blocked.
	if step.Status != schemas.StepBlocked {
		t.Errorf("expected step status %q, got %q", schemas.StepBlocked, step.Status)
	}
}

// TestHandleOutput_BudgetDeductionNilGuard verifies that handleOutput
// does not panic when result has usage info but budgetGuard is nil.
func TestHandleOutput_BudgetDeductionNilGuard(t *testing.T) {
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

	ts := store.NewTaskStore(store.NewFileTaskBackend(t.TempDir()))
	sys := &config.SystemSettings{}

	expDir := "test_exp_budget_nil"
	es := mustNewExperienceStore(t, expDir, ts, sys, nil, nil)
	defer os.RemoveAll(expDir)

	logger := mustNewLogger(t, t.TempDir(), sys)
	defer logger.Close()
	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, t.TempDir(), t.TempDir(), nil)

	// budgetGuard should be nil (not set up by NewDirectedEngine in test).
	graph := &schemas.TaskGraph{
		TaskID:          "test_budget_nil",
		GlobalWorkspace: make(map[string]any),
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", RoleID: "tester", Status: schemas.StepRunning},
		},
	}
	step := graph.Steps["s1"]

	result := &SpawnResult{
		Output: schemas.SubagentOutput{
			Status:     schemas.StatusOK,
			Confidence: 1.0,
			Result:     map[string]any{"data": "test"},
			Usage: &schemas.Usage{
				TotalTokens: 1000,
			},
		},
	}

	// Should not panic despite nil budgetGuard + non-nil Usage.
	engine.handleOutput(graph, step, result)

	// Step should be OK (high confidence, StatusOK → handleStepSuccess).
	if step.Status != schemas.StepOK {
		t.Errorf("expected step status %q, got %q", schemas.StepOK, step.Status)
	}
}
