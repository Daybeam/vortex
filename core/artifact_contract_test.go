package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestPersistGraphWritesArtifacts verifies that persistGraph computes SHA-256 +
// provenance and writes artifacts.json alongside manifest.json for primary outputs.
func TestPersistGraphWritesArtifacts(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "orch_artifacts_test")
	defer os.RemoveAll(tempDir)

	// Build an engine with outputBase = tempDir
	eng := &DirectedEngine{outputBase: tempDir}

	// Create a primary artifact file on disk
	artifactFile := filepath.Join(tempDir, "report.md")
	if err := os.WriteFile(artifactFile, []byte("# Test Report\ncontent here"), 0644); err != nil {
		t.Fatal(err)
	}

	// Graph with one primary output + one upstream step (provenance)
	upstream := &schemas.Step{
		ID:        "s1",
		Status:    schemas.StepOK,
		ResultRef: filepath.Join(tempDir, "evidence.json"),
	}
	graf := &schemas.TaskGraph{
		TaskID: "task_art",
		Steps: map[string]*schemas.Step{
			"s1": upstream,
			"s2": {ID: "s2", Status: schemas.StepOK, DependsOn: []string{"s1"}},
		},
		OutputFiles: []schemas.OutputFile{
			{StepID: "s2", Path: artifactFile, Format: "md", SizeBytes: len("# Test Report\ncontent here"), IsPrimary: true, CreatedAt: time.Now()},
		},
	}

	eng.persistGraph(graf)

	// 1. artifacts.json must exist
	aj := filepath.Join(tempDir, "task_art", "artifacts.json")
	if _, err := os.Stat(aj); os.IsNotExist(err) {
		t.Fatalf("artifacts.json not written: %v", err)
	}

	// 2. graph.Artifacts must have 1 entry with SHA-256 + source ref
	if len(graf.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(graf.Artifacts))
	}
	ac := graf.Artifacts[0]
	if ac.Path != artifactFile {
		t.Errorf("path mismatch: %q", ac.Path)
	}
	if len(ac.SHA256) != 64 {
		t.Errorf("SHA-256 should be 64 hex chars, got %q (len %d)", ac.SHA256, len(ac.SHA256))
	}
	if ac.Status != "draft" {
		t.Errorf("default status should be draft, got %q", ac.Status)
	}
	if len(ac.SourceRefs) != 1 || ac.SourceRefs[0] != upstream.ResultRef {
		t.Errorf("source refs mismatch: %v", ac.SourceRefs)
	}

	// 3. No artifacts.json when no primary outputs
	graf2 := &schemas.TaskGraph{TaskID: "task_none", Steps: map[string]*schemas.Step{}}
	eng.persistGraph(graf2)
	if _, err := os.Stat(filepath.Join(tempDir, "task_none", "artifacts.json")); !os.IsNotExist(err) {
		t.Error("artifacts.json should not exist when no primary outputs")
	}
}
