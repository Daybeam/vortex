package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// -----------------------------------------------------------------------------
// Feature: Dynamic DAG Surgery & Dead-End Node Refactoring
// Invariant: Task state is irreversible, topology integrity is preserved
// Scenario: After detecting an environmental dead-end, the node is retired
// and the topology is hot-refactored
// Source: docs/gherkin/BEHAVIOR_CONTRACTS.md §Feature: Dynamic DAG Surgery
// -----------------------------------------------------------------------------
func TestDirectedEngine_MutateGraphTopology(t *testing.T) {
	// audit T-GATE-1 (2026-10-05 r2): was skipped on Windows via
	// runtime.GOOS=="windows" → t.Skip, which meant the test NEVER RAN
	// on the development platform (搬龙门). Root cause = t.TempDir()
	// auto-cleanup races with engine bg goroutines holding logger files.
	// Fix = use mustTempDir (added by T-CRIT-1) which uses os.MkdirTemp
	// + t.Cleanup(os.RemoveAll) — same pattern as delegation_mode_test.go.
	tmpDir := mustTempDir(t, "surgery")
	reg := &config.Registry{
		System: config.SystemSettings{
			ConfidenceThreshold: 0.8,
		},
	}
	logger := mustNewLogger(t, tmpDir, nil)
	var err error
	defer logger.Close()
	engine := &DirectedEngine{
		graphs:    make(map[string]*schemas.TaskGraph),
		logger:    logger,
		registry:  reg,
		doneChans: make(map[string]chan struct{}),
	}

	graph := &schemas.TaskGraph{
		TaskID: "task1",
		Status: schemas.GraphRunning,
		Steps: map[string]*schemas.Step{
			"s1": {
				ID:        "s1",
				Status:    schemas.StepFailed,
				LastError: "Dead end reached",
			},
			"s_audit": {
				ID:        "s_audit",
				RoleID:    "auditor",
				DependsOn: []string{"s1"},
				Status:    schemas.StepPending,
			},
			"s_final": {
				ID:        "s_final",
				DependsOn: []string{"s1"},
				Status:    schemas.StepPending,
			},
		},
		ContextTree: map[string]*schemas.ContextNode{
			"root": {
				ID:      "root",
				StepIDs: []string{"s1", "s_audit", "s_final"},
			},
		},
		CurrentNodeID: "root",
	}
	engine.graphs["task1"] = graph

	surgery := SurgeryData{
		DeprecatedStepID: "s1",
		ReplacementSteps: []schemas.StepInput{
			{
				ID:        "s2",
				RoleID:    "software_engineer",
				Task:      "Replacement logic",
				DependsOn: []string{},
			},
		},
	}

	err = engine.MutateGraphTopology("task1", surgery)
	if err != nil {
		t.Fatalf("MutateGraphTopology failed: %v", err)
	}

	// 1. Verify s1 is deprecated
	if graph.Steps["s1"].Status != schemas.StepDeprecated {
		t.Errorf("expected s1 to be deprecated, got %s", graph.Steps["s1"].Status)
	}

	// 2. Verify s_audit is skipped (auto-skip)
	if graph.Steps["s_audit"].Status != schemas.StepSkipped {
		t.Errorf("expected s_audit to be skipped, got %s", graph.Steps["s_audit"].Status)
	}

	// 3. Verify s_final dependency cleanup
	if len(graph.Steps["s_final"].DependsOn) != 0 {
		t.Errorf("expected s_final to have 0 dependencies, got %d", len(graph.Steps["s_final"].DependsOn))
	}

	// 4. Verify s2 is injected
	s2, exists := graph.Steps["s2"]
	if !exists {
		t.Fatal("s2 not injected")
	}
	if !contains(graph.ContextTree["root"].StepIDs, "s2") {
		t.Error("s2 not added to ContextTree StepIDs")
	}
	if !strings.Contains(s2.Task, "[DEPRECATED PATH FAILURE REFERENCE]") {
		t.Error("s2 task description missing failure inheritance")
	}
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
