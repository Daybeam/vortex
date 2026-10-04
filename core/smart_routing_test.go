package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func buildSmartRoutingEngine(t *testing.T, requireReview bool) (*DirectedEngine, string) {
	t.Helper()
	// Avoid t.TempDir() for engine directories: background goroutines write to
	// them asynchronously; t.TempDir()'s RemoveAll fails the test on Windows.
	tmpDir, err := os.MkdirTemp("", "smart-routing-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	configPath := filepath.Join(tmpDir, "config.json")

	cfg := config.Config{
		RequirePlanReview: requireReview,
		DefaultProvider:   "mock",
		Providers: map[string]config.ProviderConfig{
			"mock": {Provider: "mock", Model: "mock-model", MaxContextWindow: 8192},
		},
		Roles: []config.Role{
			{ID: "engineer", BaseCapability: "text"},
		},
	}

	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatal(err)
	}

	ts := store.NewTaskStore(store.NewFileTaskBackend(filepath.Join(tmpDir, "tasks")))
	es := mustNewExperienceStore(t, filepath.Join(tmpDir, "exp"), ts, nil, nil, nil)
	logger := mustNewLogger(t, filepath.Join(tmpDir, "logs"), &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() }) // drain logger goroutines before tmpDir cleanup

	engine := NewDirectedEngine(reg, ts, es, nil, logger, nil, tmpDir, tmpDir, nil)
	return engine, tmpDir
}

// waitForGraph polls until the graph for taskID exists and cond returns true
// (cond == nil means just check existence). Timeout: 2s. Returns nil on timeout.
// audit T-4.1: replaces unbounded time.Sleep with bounded polling.
func waitForGraph(t *testing.T, engine *DirectedEngine, taskID string, cond func(*schemas.TaskGraph) bool) *schemas.TaskGraph {
	t.Helper()
	for i := 0; i < 20; i++ {
		engine.Mu.RLock()
		graph, exists := engine.graphs[taskID]
		// Evaluate cond(graph) under RLock to prevent data race on
		// graph.Status — the background goroutine in scheduler_submit.go
		// writes graph.Status under Mu.Lock(), so the reader must hold
		// Mu.RLock() to establish a happens-before relationship.
		done := exists && graph != nil && (cond == nil || cond(graph))
		engine.Mu.RUnlock()
		if done {
			return graph
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// TestSmartRouting_SingleStepNoDeps_BypassesDAG verifies that a single-step
// task with no dependencies is routed directly through spawner.Spawn,
// bypassing full DAG construction. A minimal graph entry and manifest.json
// are created for observability and task history visibility after restart.
func TestSmartRouting_SingleStepNoDeps_BypassesDAG(t *testing.T) {
	engine, tmpDir := buildSmartRoutingEngine(t, false)
	defer engine.Stop()

	inputs := []schemas.StepInput{
		{
			ID:             "step_fast",
			Task:           "run a single tool",
			RoleID:         "engineer",
			AdditionalMCPs: []string{"web_search"},
		},
	}

	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit() returned error: %v", err)
	}
	if taskID == "" {
		t.Fatal("Submit() returned empty taskID")
	}

	graph := waitForGraph(t, engine, taskID, nil)
	if graph == nil {
		t.Fatal("Smart-routed task should have a graph entry")
	}
	if len(graph.Steps) != 1 {
		t.Errorf("Smart-routed task should have 1 step, got %d", len(graph.Steps))
	}

	manifestPath := filepath.Join(tmpDir, taskID, "manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Errorf("Smart-routed task should write manifest.json for task history visibility: %v", err)
	}
}

// TestSmartRouting_MultiStep_UsesFullDAG verifies that multi-step tasks
// still use the full DAG pipeline.
func TestSmartRouting_MultiStep_UsesFullDAG(t *testing.T) {
	engine, _ := buildSmartRoutingEngine(t, false)
	defer engine.Stop()

	inputs := []schemas.StepInput{
		{ID: "step_1", Task: "do step 1", RoleID: "engineer", AdditionalMCPs: []string{"web_search"}},
		{ID: "step_2", Task: "do step 2", RoleID: "engineer", DependsOn: []string{"step_1"}, AdditionalMCPs: []string{"web_search"}},
	}

	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit() returned error: %v", err)
	}

	graph := waitForGraph(t, engine, taskID, func(g *schemas.TaskGraph) bool {
		return g.Status != "" && g.Status != schemas.GraphPendingReview
	})
	if graph == nil {
		t.Fatal("Multi-step task should have a graph with progressed status")
	}
	if len(graph.Steps) != 2 {
		t.Errorf("Graph steps = %d, want 2", len(graph.Steps))
	}
}

// TestSmartRouting_SingleStepWithDeps_UsesFullDAG verifies that a single-step
// task WITH dependencies still goes through full DAG.
func TestSmartRouting_SingleStepWithDeps_UsesFullDAG(t *testing.T) {
	engine, _ := buildSmartRoutingEngine(t, false)
	defer engine.Stop()

	inputs := []schemas.StepInput{
		{ID: "step_dep", Task: "do with deps", RoleID: "engineer",
			DependsOn: []string{"upstream"}, AdditionalMCPs: []string{"web_search"}},
	}

	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit() returned error: %v", err)
	}

	graph := waitForGraph(t, engine, taskID, nil)
	if graph == nil {
		t.Error("Single-step task WITH deps should use full DAG and create a graph")
	}
}

// TestSmartRouting_RequirePlanReview_DisablesFastPath verifies that when
// RequirePlanReview is enabled, all tasks go through DAG.
func TestSmartRouting_RequirePlanReview_DisablesFastPath(t *testing.T) {
	engine, _ := buildSmartRoutingEngine(t, true)
	defer engine.Stop()

	inputs := []schemas.StepInput{
		{ID: "step_review", Task: "task needing review", RoleID: "engineer",
			AdditionalMCPs: []string{"web_search"}},
	}

	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit() returned error: %v", err)
	}

	graph := waitForGraph(t, engine, taskID, func(g *schemas.TaskGraph) bool {
		return g.Status == schemas.GraphPendingReview
	})
	if graph == nil {
		t.Fatal("Task with review required should have a graph with PendingReview status")
	}
}

// TestSmartRouting_DeterministicContract_UsesFastPath verifies that a single-step
// task with a deterministic OutputContract (Path + RequiredFields) is validated
// by FastPathEngine without invoking the LLM (spawner.Spawn).
func TestSmartRouting_DeterministicContract_UsesFastPath(t *testing.T) {
	engine, tmpDir := buildSmartRoutingEngine(t, false)
	defer engine.Stop()

	artifactPath := filepath.Join(tmpDir, "artifact.json")
	if err := os.WriteFile(artifactPath, []byte(`{"name": "test", "version": "1.0"}`), 0644); err != nil {
		t.Fatal(err)
	}

	inputs := []schemas.StepInput{
		{
			ID:     "step_fast",
			Task:   "validate artifact",
			RoleID: "engineer",
			OutputContract: schemas.ArtifactContract{
				Path:           artifactPath,
				RequiredFields: []string{"name", "version"},
			},
		},
	}

	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit() error: %v", err)
	}

	graph := waitForGraph(t, engine, taskID, func(g *schemas.TaskGraph) bool {
		return g.Status == schemas.GraphCompleted
	})
	if graph == nil {
		t.Fatal("FastPath task should have a completed graph")
	}
	for _, step := range graph.Steps {
		if step.Status != schemas.StepOK {
			t.Errorf("Step status = %s, want %s", step.Status, schemas.StepOK)
		}
		if !strings.HasPrefix(step.ResultRef, "fastpath:") {
			t.Errorf("ResultRef = %q, want fastpath: prefix", step.ResultRef)
		}
	}
}

// TestSmartRouting_DeterministicContract_FallbackOnMissingFile verifies that
// FastPath failure (e.g., file not found) falls back to spawner.Spawn.
func TestSmartRouting_DeterministicContract_FallbackOnMissingFile(t *testing.T) {
	engine, tmpDir := buildSmartRoutingEngine(t, false)
	defer engine.Stop()

	missingPath := filepath.Join(tmpDir, "nonexistent.json")

	inputs := []schemas.StepInput{
		{
			ID:     "step_fallback",
			Task:   "validate missing artifact",
			RoleID: "engineer",
			OutputContract: schemas.ArtifactContract{
				Path:           missingPath,
				RequiredFields: []string{"name"},
			},
		},
	}

	taskID, err := engine.Submit(inputs)
	if err != nil {
		t.Fatalf("Submit() error: %v", err)
	}

	graph := waitForGraph(t, engine, taskID, nil)
	if graph == nil {
		t.Fatal("Fallback task should have a graph entry")
	}
	for _, step := range graph.Steps {
		if step.ResultRef != "" && strings.HasPrefix(step.ResultRef, "fastpath:") {
			t.Error("FastPath should have failed for missing file — ResultRef should not have fastpath: prefix")
		}
	}
}
