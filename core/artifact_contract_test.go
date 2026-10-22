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
	tempDir := t.TempDir()

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

// TestPersistGraph_NonExistentArtifactFile verifies that when a primary
// output file does not exist on disk, the SHA-256 hash is left empty
// (os.Open fails gracefully) and the artifact is still recorded.
func TestPersistGraph_NonExistentArtifactFile(t *testing.T) {
	tempDir := t.TempDir()
	eng := &DirectedEngine{outputBase: tempDir}

	graf := &schemas.TaskGraph{
		TaskID: "task_missing_file",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepOK},
		},
		OutputFiles: []schemas.OutputFile{
			{
				StepID:    "s1",
				Path:      filepath.Join(tempDir, "nonexistent.md"),
				Format:    "md",
				IsPrimary: true,
				CreatedAt: time.Now(),
			},
		},
	}

	eng.persistGraph(graf)

	if len(graf.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact even for missing file, got %d", len(graf.Artifacts))
	}
	ac := graf.Artifacts[0]
	if ac.SHA256 != "" {
		t.Errorf("expected empty SHA-256 for non-existent file, got %q", ac.SHA256)
	}
}

// TestPersistGraph_NonPrimaryOutputSkipped verifies that output files
// with IsPrimary=false are not included in the artifact contract.
func TestPersistGraph_NonPrimaryOutputSkipped(t *testing.T) {
	tempDir := t.TempDir()
	eng := &DirectedEngine{outputBase: tempDir}

	// Create a non-primary artifact file on disk
	artifactFile := filepath.Join(tempDir, "secondary.md")
	if err := os.WriteFile(artifactFile, []byte("secondary output"), 0644); err != nil {
		t.Fatal(err)
	}

	graf := &schemas.TaskGraph{
		TaskID: "task_non_primary",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepOK},
		},
		OutputFiles: []schemas.OutputFile{
			{StepID: "s1", Path: artifactFile, Format: "md", IsPrimary: false, CreatedAt: time.Now()},
		},
	}

	eng.persistGraph(graf)

	if len(graf.Artifacts) != 0 {
		t.Fatalf("expected 0 artifacts for non-primary output, got %d", len(graf.Artifacts))
	}

	// artifacts.json should not exist
	aj := filepath.Join(tempDir, "task_non_primary", "artifacts.json")
	if _, err := os.Stat(aj); !os.IsNotExist(err) {
		t.Error("artifacts.json should not exist when no primary outputs")
	}
}

// TestPersistGraph_MultiplePrimaryOutputs verifies that multiple primary
// outputs are all recorded as artifacts with correct SHA-256 hashes.
func TestPersistGraph_MultiplePrimaryOutputs(t *testing.T) {
	tempDir := t.TempDir()
	eng := &DirectedEngine{outputBase: tempDir}

	file1 := filepath.Join(tempDir, "report1.md")
	file2 := filepath.Join(tempDir, "report2.md")
	if err := os.WriteFile(file1, []byte("content1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file2, []byte("content2"), 0644); err != nil {
		t.Fatal(err)
	}

	graf := &schemas.TaskGraph{
		TaskID: "task_multi",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepOK},
			"s2": {ID: "s2", Status: schemas.StepOK},
		},
		OutputFiles: []schemas.OutputFile{
			{StepID: "s1", Path: file1, Format: "md", IsPrimary: true, CreatedAt: time.Now()},
			{StepID: "s2", Path: file2, Format: "md", IsPrimary: true, CreatedAt: time.Now()},
		},
	}

	eng.persistGraph(graf)

	if len(graf.Artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(graf.Artifacts))
	}

	for i, ac := range graf.Artifacts {
		if len(ac.SHA256) != 64 {
			t.Errorf("artifact[%d]: expected 64-char SHA-256, got %q (len %d)", i, ac.SHA256, len(ac.SHA256))
		}
		if ac.Status != "draft" {
			t.Errorf("artifact[%d]: expected status draft, got %q", i, ac.Status)
		}
	}
}

// TestPersistGraph_Debounce verifies that calling persistGraph twice
// within the 200ms debounce window skips the second call.
func TestPersistGraph_Debounce(t *testing.T) {
	tempDir := t.TempDir()
	eng := &DirectedEngine{outputBase: tempDir}

	artifactFile := filepath.Join(tempDir, "report.md")
	if err := os.WriteFile(artifactFile, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	graf := &schemas.TaskGraph{
		TaskID: "task_debounce",
		Steps: map[string]*schemas.Step{
			"s1": {ID: "s1", Status: schemas.StepOK},
		},
		OutputFiles: []schemas.OutputFile{
			{StepID: "s1", Path: artifactFile, Format: "md", IsPrimary: true, CreatedAt: time.Now()},
		},
	}

	// First call: should persist and set graph.Artifacts
	eng.persistGraph(graf)
	if len(graf.Artifacts) != 1 {
		t.Fatalf("first call: expected 1 artifact, got %d", len(graf.Artifacts))
	}

	// Add a second primary output to the graph
	artifactFile2 := filepath.Join(tempDir, "report2.md")
	if err := os.WriteFile(artifactFile2, []byte("content2"), 0644); err != nil {
		t.Fatal(err)
	}
	graf.OutputFiles = append(graf.OutputFiles, schemas.OutputFile{
		StepID:    "s1",
		Path:      artifactFile2,
		Format:    "md",
		IsPrimary: true,
		CreatedAt: time.Now(),
	})

	// Second call immediately: should be debounced (within 200ms).
	// graph.Artifacts should NOT be updated to 2 entries.
	eng.persistGraph(graf)
	if len(graf.Artifacts) != 1 {
		t.Errorf("debounced call should not update artifacts: expected 1, got %d", len(graf.Artifacts))
	}
}
