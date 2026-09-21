package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestDirectedEngine_ASAE(t *testing.T) {
	tmpDir := t.TempDir()
	logger, _ := NewLogger(tmpDir, nil)
	defer logger.Close()
	reg := &config.Registry{
		System: config.SystemSettings{
			ConfidenceThreshold: 0.8,
			MaxSpawnDepth:       2,
		},
	}

	engine := &DirectedEngine{
		graphs:     make(map[string]*schemas.TaskGraph),
		logger:     logger,
		registry:   reg,
		doneChans:  make(map[string]chan struct{}),
		outputBase: tmpDir,
	}

	t.Run("SpawningOnLowConfidence", func(t *testing.T) {
		taskID := "task_lc"
		step := &schemas.Step{
			ID:         "s1",
			Task:       "Original task",
			Status:     schemas.StepRunning,
			SpawnDepth: 0,
		}
		graph := &schemas.TaskGraph{
			TaskID: taskID,
			Steps:  map[string]*schemas.Step{"s1": step},
			Status: schemas.GraphRunning,
			ContextTree: map[string]*schemas.ContextNode{
				"root": {ID: "root", StepIDs: []string{"s1"}},
			},
			CurrentNodeID: "root",
		}
		engine.graphs[taskID] = graph
		engine.doneChans[taskID] = make(chan struct{})

		result := &SpawnResult{
			Output: schemas.SubagentOutput{
				Status:     schemas.StatusPartial,
				Confidence: 0.5, // Low confidence
			},
		}

		engine.handleOutput(graph, step, result)

		// Verify ASAE triggered spawning
		if step.Status != schemas.StepDeprecated {
			t.Errorf("Expected step to be deprecated, got %s", step.Status)
		}

		// Check if a new step was injected
		spawnedFound := false
		var spawnedStep *schemas.Step
		for id, s := range graph.Steps {
			if id != "s1" && s.SpawnDepth == 1 {
				spawnedFound = true
				spawnedStep = s
				break
			}
		}

		if !spawnedFound {
			t.Fatal("Expected spawned step not found")
		}

		if spawnedStep.MaxSpawnDepth != 2 {
			t.Errorf("Expected spawned step MaxSpawnDepth 2, got %d", spawnedStep.MaxSpawnDepth)
		}
	})

	t.Run("SpawningOnMissingContext", func(t *testing.T) {
		taskID := "task_mc"
		step := &schemas.Step{
			ID:         "s1",
			Task:       "Original task",
			Status:     schemas.StepRunning,
			SpawnDepth: 0,
		}
		graph := &schemas.TaskGraph{
			TaskID: taskID,
			Steps:  map[string]*schemas.Step{"s1": step},
			Status: schemas.GraphRunning,
			ContextTree: map[string]*schemas.ContextNode{
				"root": {ID: "root", StepIDs: []string{"s1"}},
			},
			CurrentNodeID: "root",
		}
		engine.graphs[taskID] = graph
		engine.doneChans[taskID] = make(chan struct{})

		result := &SpawnResult{
			Output: schemas.SubagentOutput{
				Status:         schemas.StatusPartial,
				Confidence:     0.9,
				MissingContext: []string{"missing_api_key"},
			},
		}

		engine.handleOutput(graph, step, result)

		// Verify ASAE triggered spawning
		if step.Status != schemas.StepDeprecated {
			t.Errorf("Expected step to be deprecated, got %s", step.Status)
		}

		// Check if a new step was injected
		spawnedFound := false
		for id, s := range graph.Steps {
			if id != "s1" && s.SpawnDepth == 1 {
				spawnedFound = true
				break
			}
		}

		if !spawnedFound {
			t.Fatal("Expected spawned step not found")
		}
	})

	t.Run("MaxDepthGuard", func(t *testing.T) {
		taskID := "task_guard"
		step := &schemas.Step{
			ID:            "s1",
			Task:          "Original task",
			Status:        schemas.StepRunning,
			SpawnDepth:    2, // At max depth
			MaxSpawnDepth: 2,
		}
		graph := &schemas.TaskGraph{
			TaskID: taskID,
			Steps:  map[string]*schemas.Step{"s1": step},
			Status: schemas.GraphRunning,
			ContextTree: map[string]*schemas.ContextNode{
				"root": {ID: "root", StepIDs: []string{"s1"}},
			},
			CurrentNodeID: "root",
		}
		engine.graphs[taskID] = graph
		engine.doneChans[taskID] = make(chan struct{})

		result := &SpawnResult{
			Output: schemas.SubagentOutput{
				Status:         schemas.StatusPartial,
				Confidence:     0.5,
				MissingContext: []string{"deep_info"},
			},
		}

		engine.handleOutput(graph, step, result)

		// Verify ASAE did NOT spawn, but blocked with decision
		if step.Status != schemas.StepBlocked {
			t.Errorf("Expected step to be blocked at max depth, got %s", step.Status)
		}

		if len(graph.PendingDecisions) == 0 {
			t.Fatal("Expected decision to be created at max depth")
		}

		// Verify no spawned steps
		for id := range graph.Steps {
			if id != "s1" {
				t.Errorf("Unexpected step %s found", id)
			}
		}
	})

	t.Run("DependencyCleanupAndHeritage", func(t *testing.T) {
		taskID := "task_dep"
		step := &schemas.Step{
			ID:         "s1",
			Task:       "Original task",
			Status:     schemas.StepRunning,
			LastError:  "Original error",
			SpawnDepth: 0,
		}
		downstream := &schemas.Step{
			ID:        "s_down",
			Task:      "Downstream task",
			DependsOn: []string{"s1"},
			Status:    schemas.StepPending,
		}
		auditor := &schemas.Step{
			ID:        "s_audit",
			RoleID:    "auditor",
			Task:      "Audit s1",
			DependsOn: []string{"s1"},
			Status:    schemas.StepPending,
		}

		graph := &schemas.TaskGraph{
			TaskID: taskID,
			Steps:  map[string]*schemas.Step{"s1": step, "s_down": downstream, "s_audit": auditor},
			Status: schemas.GraphRunning,
			ContextTree: map[string]*schemas.ContextNode{
				"root": {ID: "root", StepIDs: []string{"s1", "s_down", "s_audit"}},
			},
			CurrentNodeID: "root",
		}
		engine.graphs[taskID] = graph
		engine.doneChans[taskID] = make(chan struct{})

		result := &SpawnResult{
			Output: schemas.SubagentOutput{
				Status:     schemas.StatusPartial,
				Confidence: 0.5,
			},
		}

		engine.handleOutput(graph, step, result)

		// 1. Verify s1 is deprecated
		if step.Status != schemas.StepDeprecated {
			t.Errorf("Expected s1 to be deprecated, got %s", step.Status)
		}

		// 2. Verify auditor is skipped
		if auditor.Status != schemas.StepSkipped {
			t.Errorf("Expected auditor to be skipped, got %s", auditor.Status)
		}

		// 3. Verify downstream dependency cleanup
		if len(downstream.DependsOn) != 0 {
			t.Errorf("Expected downstream to have 0 deps, got %d", len(downstream.DependsOn))
		}

		// 4. Verify spawned step heritage
		var spawnedStep *schemas.Step
		for id, s := range graph.Steps {
			if id != "s1" && id != "s_down" && id != "s_audit" {
				spawnedStep = s
				break
			}
		}

		if spawnedStep == nil {
			t.Fatal("Spawned step not found")
		}

		if !strings.Contains(spawnedStep.Task, "[DEPRECATED PATH FAILURE REFERENCE]") {
			t.Error("Spawned step missing failure heritage")
		}
		if !strings.Contains(spawnedStep.Task, "Original error") {
			t.Error("Spawned step missing original error in heritage")
		}
	})

	t.Run("RefinedTaskContainsMissingContext", func(t *testing.T) {
		taskID := "task_refined"
		step := &schemas.Step{
			ID:     "s1",
			Task:   "Process data",
			Status: schemas.StepRunning,
		}
		graph := &schemas.TaskGraph{
			TaskID: taskID,
			Steps:  map[string]*schemas.Step{"s1": step},
			Status: schemas.GraphRunning,
			ContextTree: map[string]*schemas.ContextNode{
				"root": {ID: "root", StepIDs: []string{"s1"}},
			},
			CurrentNodeID: "root",
		}
		engine.graphs[taskID] = graph
		engine.doneChans[taskID] = make(chan struct{})

		result := &SpawnResult{
			Output: schemas.SubagentOutput{
				Status:         schemas.StatusPartial,
				Confidence:     0.9,
				MissingContext: []string{"upstream_report_v2"},
			},
		}

		engine.handleOutput(graph, step, result)

		var spawnedStep *schemas.Step
		for id, s := range graph.Steps {
			if id != "s1" && id != "s_down" && id != "s_audit" {
				spawnedStep = s
				break
			}
		}

		if spawnedStep == nil {
			t.Fatal("Spawned step not found")
		}

		if !strings.Contains(spawnedStep.Task, "upstream_report_v2") {
			t.Errorf("Expected refined task to contain missing context 'upstream_report_v2', got %q", spawnedStep.Task)
		}
	})
}
