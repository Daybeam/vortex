package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	base := "/tmp/test_task"

	tests := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{"simple_relative", "report.json", false},
		{"nested_relative", "sub/dir/output.txt", false},
		{"dot_current", "./report.json", false},
		{"traversal_parent", "../escape.txt", true},
		{"traversal_deep", "../../etc/passwd", true},
		{"absolute_path", "/etc/passwd", true},
		{"windows_absolute", "C:\\Windows\\system32", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := safeJoin(base, tt.rel)
			if (err != nil) != tt.wantErr {
				t.Errorf("safeJoin(%q) error = %v, wantErr %v", tt.rel, err, tt.wantErr)
			}
		})
	}
}

func TestHandleCoreTool_WriteAndReadFile(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_task_123"

	// write_file
	result, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "report.json",
		"content": `{"status": "ok", "value": 42}`,
	}, tmpDir, taskID)
	if err != nil {
		t.Fatalf("write_file failed: %v", err)
	}
	if !strContains(result, "Written") {
		t.Errorf("unexpected write result: %s", result)
	}

	// Verify file exists
	writtenPath := filepath.Join(tmpDir, taskID, "report.json")
	data, err := os.ReadFile(writtenPath)
	if err != nil {
		t.Fatalf("file not found: %v", err)
	}
	if string(data) != `{"status": "ok", "value": 42}` {
		t.Errorf("unexpected file content: %s", string(data))
	}

	// read_file
	result, err = HandleCoreTool(context.Background(), "read_file", map[string]any{
		"path": "report.json",
	}, tmpDir, taskID)
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	if !strContains(result, `"status": "ok"`) {
		t.Errorf("unexpected read result: %s", result)
	}
}

func TestHandleCoreTool_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_task_456"

	_, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "../../../etc/passwd",
		"content": "malicious",
	}, tmpDir, taskID)
	if err == nil {
		t.Error("expected error for path traversal, got nil")
	}

	_, err = HandleCoreTool(context.Background(), "read_file", map[string]any{
		"path": "../../../etc/passwd",
	}, tmpDir, taskID)
	if err == nil {
		t.Error("expected error for path traversal read, got nil")
	}
}

func TestHandleCoreTool_NestedDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_task_789"

	_, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "sub/dir/output.txt",
		"content": "nested content",
	}, tmpDir, taskID)
	if err != nil {
		t.Fatalf("write to nested dir failed: %v", err)
	}

	writtenPath := filepath.Join(tmpDir, taskID, "sub", "dir", "output.txt")
	data, err := os.ReadFile(writtenPath)
	if err != nil {
		t.Fatalf("file not found: %v", err)
	}
	if string(data) != "nested content" {
		t.Errorf("unexpected content: %s", string(data))
	}
}

func TestHandleCoreTool_UnknownTool(t *testing.T) {
	_, err := HandleCoreTool(context.Background(), "nonexistent_tool", map[string]any{}, "/tmp", "task")
	if err == nil {
		t.Error("expected error for unknown tool")
	}
}

func strContains(s, substr string) bool {
	return len(s) >= len(substr) && (strIndexOf(s, substr) >= 0)
}

func strIndexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
