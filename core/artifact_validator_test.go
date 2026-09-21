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
