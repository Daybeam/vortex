package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

func TestDirectedEngine_DetectReadDeps_Versioning(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "coordination_version_test")
	defer os.RemoveAll(tempDir)

	taskID := "task_v1"
	taskDir := filepath.Join(tempDir, taskID)
	_ = os.MkdirAll(taskDir, 0755)

	reg := &config.Registry{
		System: config.SystemSettings{
			StagingEnabled: true,
		},
	}
	engine := &DirectedEngine{
		registry:   reg,
		outputBase: tempDir,
	}
	engine.walker = NewDAGGraphWalker(nil, nil, tempDir, nil, reg)

	// 1. Create a producer step output
	filePath := filepath.Join(taskDir, "output.txt")
	content := []byte("v1 content")
	_ = os.WriteFile(filePath, content, 0644)

	graph := &schemas.TaskGraph{
		TaskID: taskID,
		OutputFiles: []schemas.OutputFile{
			{StepID: "step1", Path: filePath},
		},
	}

	// 2. Take a snapshot
	sw := NewStagedWorkspace(taskDir)
	snap, err := sw.TakePreStepSnapshot("step2")
	if err != nil {
		t.Fatalf("TakePreStepSnapshot failed: %v", err)
	}

	// 3. Mock a trace reading this file
	trace := []store.ToolInteraction{
		{
			ToolName: "read_file",
			Arguments: map[string]any{
				"path": filePath,
			},
		},
	}

	// 4. Detect deps
	deps := engine.detectReadDeps(graph, "step2", trace, snap)

	if len(deps) != 1 {
		t.Fatalf("expected 1 dependency, got %d", len(deps))
	}

	dep := deps[0]
	if dep.StepID != "step1" {
		t.Errorf("wrong producer step: %s", dep.StepID)
	}
	if dep.SnapshotID == "" {
		t.Errorf("SnapshotID missing")
	}

	expectedHash := hashBytes(content)
	if dep.SourceSHA256 != expectedHash {
		t.Errorf("SHA256 mismatch: got %s, want %s", dep.SourceSHA256, expectedHash)
	}
}
