package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestHandleStepSuccess_ContractFileExists_RegistersOutput(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	wsRoot := t.TempDir()
	contractPath := "report.json"
	fullPath := filepath.Join(wsRoot, contractPath)
	os.WriteFile(fullPath, []byte(`{"status":"ok"}`), 0644)

	graph := &schemas.TaskGraph{
		TaskID:        "task-contract-exists",
		WorkspaceRoot: wsRoot,
		Steps:         make(map[string]*schemas.Step),
		Status:        schemas.GraphRunning,
	}
	step := &schemas.Step{
		ID:     "s1",
		Status: schemas.StepRunning,
		RoleID: "r1",
		OutputContract: schemas.ArtifactContract{
			Path:   contractPath,
			Format: "json",
		},
	}
	graph.Steps["s1"] = step
	injectTestGraph(t, s, graph)

	result := &SpawnResult{Output: schemas.SubagentOutput{
		Status:     schemas.StatusOK,
		Confidence: 0.9,
		Result:     map[string]any{},
	}}

	s.handleStepSuccess(graph, step, result)

	s.Mu.RLock()
	defer s.Mu.RUnlock()
	found := false
	for _, of := range graph.OutputFiles {
		if of.StepID == "s1" && of.Path == contractPath {
			found = true
			if of.SizeBytes <= 0 {
				t.Errorf("expected non-zero size, got %d", of.SizeBytes)
			}
			if of.Format != "json" {
				t.Errorf("format = %q, want %q", of.Format, "json")
			}
			break
		}
	}
	if !found {
		t.Fatalf("output_files does not contain contract file %q: %+v", contractPath, graph.OutputFiles)
	}
}

func TestHandleStepSuccess_ContractFileMissing_DoesNotRegisterButStepSucceeds(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	wsRoot := t.TempDir()

	graph := &schemas.TaskGraph{
		TaskID:        "task-contract-missing",
		WorkspaceRoot: wsRoot,
		Steps:         make(map[string]*schemas.Step),
		Status:        schemas.GraphRunning,
	}
	step := &schemas.Step{
		ID:     "s1",
		Status: schemas.StepRunning,
		RoleID: "r1",
		OutputContract: schemas.ArtifactContract{
			Path: "nonexistent.json",
		},
	}
	graph.Steps["s1"] = step
	injectTestGraph(t, s, graph)

	result := &SpawnResult{Output: schemas.SubagentOutput{
		Status:     schemas.StatusOK,
		Confidence: 0.9,
		Result:     map[string]any{},
	}}

	s.handleStepSuccess(graph, step, result)

	s.Mu.RLock()
	defer s.Mu.RUnlock()

	for _, of := range graph.OutputFiles {
		if of.Path == "nonexistent.json" {
			t.Fatalf("missing contract file should not be registered in output_files: %+v", graph.OutputFiles)
		}
	}

	if step.Status != schemas.StepOK {
		t.Errorf("step status = %q, want %q (registration failure must not fail step)", step.Status, schemas.StepOK)
	}
}

func TestHandleStepSuccess_NoContract_NoRegistration(t *testing.T) {
	s, _ := newAbortTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-no-contract",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{
		ID:     "s1",
		Status: schemas.StepRunning,
		RoleID: "r1",
	}
	graph.Steps["s1"] = step
	injectTestGraph(t, s, graph)

	result := &SpawnResult{Output: schemas.SubagentOutput{
		Status:     schemas.StatusOK,
		Confidence: 0.9,
		Result:     map[string]any{},
	}}

	s.handleStepSuccess(graph, step, result)

	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if len(graph.OutputFiles) != 0 {
		t.Errorf("expected no output files for step without contract, got %+v", graph.OutputFiles)
	}
}
