package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestVerifyExitCriteria_ArtifactContract_Passes — positive path:
// step declares OutputContract, on-disk artifact satisfies it → PASS before
// any VerifierModel audit runs.
func TestVerifyExitCriteria_ArtifactContract_Passes(t *testing.T) {
	tmpDir := t.TempDir()
	artPath := filepath.Join(tmpDir, "output.json")
	doc := map[string]interface{}{
		"name":     "alice",
		"count":    float64(42),
		"items":    []interface{}{"a", "b"},
		"metadata": map[string]interface{}{"k": "v"},
	}
	raw, _ := json.Marshal(doc)
	os.WriteFile(artPath, raw, 0644)

	graph := &schemas.TaskGraph{
		OutputFiles: []schemas.OutputFile{
			{StepID: "step1", Path: artPath, IsPrimary: true},
		},
	}
	step := &schemas.Step{
		ID: "step1",
		OutputContract: schemas.ArtifactContract{
			RequiredFields: []string{"name", "count", "items", "metadata"},
			TypeSchema:     map[string]string{"name": "string", "count": "number", "items": "array", "metadata": "object"},
		},
	}

	ok, _, failType := validateArtifactContract(graph, step)
	if !ok {
		t.Fatalf("expected PASS, got failType=%s", failType)
	}
}

// TestVerifyExitCriteria_ArtifactContract_Violation — deterministic shield:
// artifact missing a RequiredField → FailureSchemaViolation.
func TestVerifyExitCriteria_ArtifactContract_Violation(t *testing.T) {
	tmpDir := t.TempDir()
	artPath := filepath.Join(tmpDir, "output.json")
	os.WriteFile(artPath, []byte(`{"name":"bob"}`), 0644)

	graph := &schemas.TaskGraph{
		OutputFiles: []schemas.OutputFile{
			{StepID: "step1", Path: artPath, IsPrimary: true},
		},
	}
	step := &schemas.Step{
		ID: "step1",
		OutputContract: schemas.ArtifactContract{
			RequiredFields: []string{"name", "count", "risk_level"},
		},
	}

	ok, reason, failType := validateArtifactContract(graph, step)
	if ok {
		t.Fatal("expected FAIL for missing required field")
	}
	if failType != schemas.FailureSchemaViolation {
		t.Errorf("expected schema_violation, got %s", failType)
	}
	// ValidateArtifact reports the FIRST missing field, so the reason
	// string will mention "count" (first in the RequiredFields slice),
	// not "risk_level". Assert on the field that actually gets reported.
	if !strings.Contains(reason, "count") && !strings.Contains(reason, "risk_level") {
		t.Errorf("expected a missing required field in reason, got: %s", reason)
	}
}

// TestVerifyExitCriteria_ArtifactContract_NoContract_NoOp — backward compat:
// step with no OutputContract skips the deterministic check entirely.
func TestVerifyExitCriteria_ArtifactContract_NoContract_NoOp(t *testing.T) {
	tmpDir := t.TempDir()
	artPath := filepath.Join(tmpDir, "output.txt")
	os.WriteFile(artPath, []byte("not json at all"), 0644)

	graph := &schemas.TaskGraph{
		OutputFiles: []schemas.OutputFile{
			{StepID: "step1", Path: artPath, IsPrimary: true},
		},
	}
	step := &schemas.Step{ID: "step1"} // empty OutputContract

	ok, _, failType := validateArtifactContract(graph, step)
	if !ok {
		t.Fatalf("no contract should PASS, got failType=%s", failType)
	}
}

// fixes audit T-C05: the original verifyContractOnly was a verbatim copy of
// the Case 1 block from verifyExitCriteria. Tests now call the production
// function validateArtifactContract directly, so they will FAIL if the
// production logic diverges.
