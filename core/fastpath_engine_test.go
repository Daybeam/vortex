package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestFastPathEngine_ExecuteFastPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "fastpath_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	engine := NewFastPathEngine(tmpDir)
	ctx := context.Background()

	// Test Case 1: Success path
	artifactPath := filepath.Join(tmpDir, "result.json")
	content := `{"status": "ok", "score": 95}`
	if err := os.WriteFile(artifactPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	contract := schemas.ArtifactContract{
		Path:           artifactPath,
		RequiredFields: []string{"status"},
		TypeSchema:     map[string]string{"score": "number"},
	}

	res, err := engine.ExecuteFastPath(ctx, contract, nil)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	m, ok := res.(map[string]any)
	if !ok || m["status"] != "success" || m["fastpath"] != true {
		t.Errorf("unexpected response: %v", res)
	}

	// Test Case 2: Contract Violation
	contract.RequiredFields = append(contract.RequiredFields, "missing_field")
	_, err = engine.ExecuteFastPath(ctx, contract, nil)
	if err == nil {
		t.Error("expected error for contract violation, got nil")
	}

	// Test Case 3: Path Escape
	contract.Path = "../../etc/passwd"
	_, err = engine.ExecuteFastPath(ctx, contract, nil)
	if err == nil {
		t.Error("expected error for path escape, got nil")
	}
}
