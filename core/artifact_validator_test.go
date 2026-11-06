package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestValidateArtifact_NoConstraints_NoOp(t *testing.T) {
	tmpDir := t.TempDir()
	artPath := filepath.Join(tmpDir, "data.json")
	os.WriteFile(artPath, []byte(`{"anything":"goes"}`), 0644)

	ac := schemas.ArtifactContract{Path: artPath, RequiredFields: nil, TypeSchema: nil}
	if err := ValidateArtifact(ac); err != nil {
		t.Fatalf("no constraints should pass, got: %v", err)
	}
}

func TestValidateArtifact_MissingRequiredField(t *testing.T) {
	tmpDir := t.TempDir()
	artPath := filepath.Join(tmpDir, "data.json")
	os.WriteFile(artPath, []byte(`{"foo":"bar"}`), 0644)

	ac := schemas.ArtifactContract{
		Path:           artPath,
		RequiredFields: []string{"foo", "missing_field"},
	}
	err := ValidateArtifact(ac)
	if err == nil {
		t.Fatal("expected error for missing required field")
	}
	if !strings.Contains(err.Error(), string(schemas.FailureSchemaViolation)) {
		t.Errorf("expected schema_violation, got: %v", err)
	}
}

func TestValidateArtifact_TypeMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	artPath := filepath.Join(tmpDir, "data.json")
	os.WriteFile(artPath, []byte(`{"name":"Alice","age":"twenty-nine"}`), 0644)

	ac := schemas.ArtifactContract{
		Path:       artPath,
		TypeSchema: map[string]string{"name": "string", "age": "number"},
	}
	err := ValidateArtifact(ac)
	if err == nil {
		t.Fatal("expected error for type mismatch")
	}
	if !strings.Contains(err.Error(), "age") {
		t.Errorf("expected 'age' in error, got: %v", err)
	}
}

func TestValidateArtifact_AllPass(t *testing.T) {
	tmpDir := t.TempDir()
	artPath := filepath.Join(tmpDir, "data.json")
	doc := map[string]interface{}{
		"name":      "test",
		"count":     float64(42),
		"items":     []interface{}{"a", "b"},
		"metadata":  map[string]interface{}{"key": "val"},
		"active":    true,
		"deletable": nil,
	}
	raw, _ := json.Marshal(doc)
	os.WriteFile(artPath, raw, 0644)

	ac := schemas.ArtifactContract{
		Path:           artPath,
		RequiredFields: []string{"name", "count", "items", "metadata", "active", "deletable"},
		TypeSchema:     map[string]string{"name": "string", "count": "number", "items": "array", "metadata": "object", "active": "boolean", "deletable": "null"},
	}
	if err := ValidateArtifact(ac); err != nil {
		t.Fatalf("all-pass expected nil, got: %v", err)
	}
}

func TestValidateArtifact_NotJSON(t *testing.T) {
	tmpDir := t.TempDir()
	artPath := filepath.Join(tmpDir, "data.txt")
	os.WriteFile(artPath, []byte("just plain text, not json at all"), 0644)

	ac := schemas.ArtifactContract{
		Path:           artPath,
		RequiredFields: []string{"must_exist"},
	}
	err := ValidateArtifact(ac)
	if err == nil {
		t.Fatal("expected error for non-JSON content")
	}
	if !strings.Contains(err.Error(), string(schemas.FailureSchemaViolation)) {
		t.Errorf("expected schema_violation, got: %v", err)
	}
}

// InspectArtifact is the "tell me everything that is wrong" half that the
// completion contract needs; ValidateArtifact is the gate and stops at the first
// problem. They must not drift apart, so this test pins both at once: every
// problem InspectArtifact reports, and the exact message ValidateArtifact still
// produces for the first one.
func TestInspectArtifact_ReportsEveryProblemAndValidateAgrees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(`{"a":1,"c":"not an object"}`), 0644); err != nil {
		t.Fatal(err)
	}
	ac := schemas.ArtifactContract{
		Path:           path,
		RequiredFields: []string{"a", "b"},
		TypeSchema:     map[string]string{"c": "object"},
	}

	problems := InspectArtifact(ac)
	got := make(map[string]ArtifactProblemReason, len(problems))
	for _, p := range problems {
		got[p.Field] = p.Reason
	}
	if got["b"] != ProblemMissing {
		t.Errorf("field b reason = %q, want %q (problems: %#v)", got["b"], ProblemMissing, problems)
	}
	if got["c"] != ProblemWrongType {
		t.Errorf("field c reason = %q, want %q (problems: %#v)", got["c"], ProblemWrongType, problems)
	}
	if len(problems) != 2 {
		t.Errorf("problems = %d, want 2 (one per breach, not one per artifact)", len(problems))
	}

	// Ordering is part of the contract: RequiredFields are reported before
	// TypeSchema, so the gate's message is stable across runs.
	if problems[0].Field != "b" {
		t.Errorf("first problem = %q, want the required field b", problems[0].Field)
	}

	err := ValidateArtifact(ac)
	if err == nil {
		t.Fatal("gate must reject the same artifact")
	}
	if !strings.Contains(err.Error(), string(schemas.FailureSchemaViolation)) ||
		!strings.Contains(err.Error(), `"b"`) {
		t.Errorf("gate message = %v, want it to name the first breach (field b)", err)
	}

	// Field declared only in TypeSchema keeps its distinct message: the two
	// messages tell a caller different things, so collapsing them would be a
	// silent behaviour change for the Orchestrator handoff path.
	onlyType := schemas.ArtifactContract{Path: path, TypeSchema: map[string]string{"zzz": "string"}}
	err = ValidateArtifact(onlyType)
	if err == nil || !strings.Contains(err.Error(), "declared in type_schema but absent") {
		t.Errorf("type-schema-only absence = %v, want the type_schema message", err)
	}
}
