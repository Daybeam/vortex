package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// ─── Defect 8: execute_code silent success (harness-bench report §3.8) ───

// TestExecuteCode_SilentSuccess_ReportsFileChanges verifies that when
// execute_code succeeds with empty stdout, the response includes a list
// of files modified during execution. Without this, the model can't
// observe whether anything happened → loops until max_turns.
//
// Bug: 041-capacity-planning task had 38/41空转 calls because the script
// writes 3 deliverables but never prints — model sees empty stdout and
// re-runs the same script 37 times.
func TestExecuteCode_SilentSuccess_ReportsFileChanges(t *testing.T) {
	tmpDir := t.TempDir()

	// Python script that writes a file but produces no stdout.
	code := `
import os
with open(os.path.join(os.getcwd(), "deliverable.txt"), "w") as f:
    f.write("task complete")
`

	result, err := executeCode(context.Background(), code, "python", tmpDir)
	if err != nil {
		t.Skipf("python not available: %v", err)
	}

	stdout, _ := result["stdout"].(string)
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}

	filesModified, ok := result["files_modified"].([]string)
	if !ok {
		t.Fatalf("expected files_modified in response, got %v", result)
	}

	found := false
	for _, f := range filesModified {
		if f == "deliverable.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("deliverable.txt not in files_modified: %v", filesModified)
	}

	hint, _ := result["hint"].(string)
	if !strings.Contains(hint, "file") {
		t.Errorf("hint should mention files, got %q", hint)
	}
}

// TestExecuteCode_SilentSuccess_NoChanges_AddsHint verifies that when
// execute_code succeeds with empty stdout AND no file changes, the
// response includes a hint suggesting print() statements.
func TestExecuteCode_SilentSuccess_NoChanges_AddsHint(t *testing.T) {
	tmpDir := t.TempDir()

	// Python script that does nothing observable.
	code := `x = 1 + 1`

	result, err := executeCode(context.Background(), code, "python", tmpDir)
	if err != nil {
		t.Skipf("python not available: %v", err)
	}

	hint, _ := result["hint"].(string)
	if !strings.Contains(hint, "print") {
		t.Errorf("hint should suggest print() statements, got %q", hint)
	}
}

// ─── Defect 9: error message includes allowed directory (harness-bench §3.9) ───

// TestHandleCoreTool_ErrorIncludesAllowedDir verifies that when a path
// is rejected for being outside allowed directories, the error message
// names which directories ARE allowed. Without this, the model can only
// guess where it's allowed to write.
func TestHandleCoreTool_ErrorIncludesAllowedDir(t *testing.T) {
	tmpDir := t.TempDir()
	sessionRoot := t.TempDir()

	validator := NewSafePathValidator(tmpDir, sessionRoot, []string{sessionRoot})

	// Try to write to a path outside allowed directories.
	// Use a path that's absolute on both Windows and Unix.
	outsidePath := filepath.Join(os.TempDir(), "definitely_outside_allowed_dir_test.txt")
	_, err := HandleCoreTool(context.Background(), "write_file",
		map[string]any{"path": outsidePath, "content": "test"},
		tmpDir, "test-task", validator)

	if err == nil {
		t.Fatal("expected error for path outside allowed directories")
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "allowed:") {
		t.Errorf("error should list allowed directories, got: %s", errStr)
	}
	if !strings.Contains(errStr, sessionRoot) {
		t.Errorf("error should include session root %q, got: %s", sessionRoot, errStr)
	}
}

// ─── Defect 7: batch_replace core tool wiring (harness-bench §3.7) ───

// TestHandleCoreTool_BatchReplace_Single verifies that the batch_replace
// tool is wired into HandleCoreTool and can perform a single text
// replacement. This is a regression test for the orphaned
// ExecuteBatchReplace function (core/replace_batch.go) that was
// implemented but never exposed as a core tool.
func TestHandleCoreTool_BatchReplace_Single(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_batch_replace_1"

	// Create a file to edit.
	_, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "config.yaml",
		"content": "name: myapp\nversion: 1.0.0\nport: 8080\n",
	}, tmpDir, taskID)
	if err != nil {
		t.Fatalf("setup write_file failed: %v", err)
	}

	// Replace version.
	result, err := HandleCoreTool(context.Background(), "batch_replace", map[string]any{
		"replacements": []any{
			map[string]any{
				"file_path": "config.yaml",
				"old_str":   "version: 1.0.0",
				"new_str":   "version: 2.0.0",
			},
		},
	}, tmpDir, taskID)
	if err != nil {
		t.Fatalf("batch_replace failed: %v", err)
	}
	if !strContains(result, "success") && !strContains(result, "Success") {
		t.Errorf("expected success in result, got: %s", result)
	}

	// Verify the file was actually changed.
	data, err := os.ReadFile(filepath.Join(tmpDir, taskID, "config.yaml"))
	if err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if !strContains(string(data), "version: 2.0.0") {
		t.Errorf("file should contain new version, got: %s", string(data))
	}
	if strContains(string(data), "version: 1.0.0") {
		t.Errorf("file should not contain old version, got: %s", string(data))
	}
}

// TestHandleCoreTool_BatchReplace_MultiFile verifies batch_replace can
// edit multiple files in a single atomic operation.
func TestHandleCoreTool_BatchReplace_MultiFile(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_batch_replace_2"

	// Create two files.
	for _, f := range []struct{ path, content string }{
		{"a.txt", "alpha=1\nbeta=2\n"},
		{"b.txt", "gamma=3\ndelta=4\n"},
	} {
		_, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
			"path":    f.path,
			"content": f.content,
		}, tmpDir, taskID)
		if err != nil {
			t.Fatalf("setup write_file %s failed: %v", f.path, err)
		}
	}

	// Replace in both files atomically.
	_, err := HandleCoreTool(context.Background(), "batch_replace", map[string]any{
		"replacements": []any{
			map[string]any{"file_path": "a.txt", "old_str": "alpha=1", "new_str": "alpha=10"},
			map[string]any{"file_path": "b.txt", "old_str": "gamma=3", "new_str": "gamma=30"},
		},
	}, tmpDir, taskID)
	if err != nil {
		t.Fatalf("batch_replace multi-file failed: %v", err)
	}

	// Both files should be updated.
	for _, f := range []struct{ path, expect string }{
		{"a.txt", "alpha=10"},
		{"b.txt", "gamma=30"},
	} {
		data, _ := os.ReadFile(filepath.Join(tmpDir, taskID, f.path))
		if !strContains(string(data), f.expect) {
			t.Errorf("%s should contain %q, got: %s", f.path, f.expect, string(data))
		}
	}
}

// TestHandleCoreTool_BatchReplace_OldStrNotFound verifies that when
// old_str doesn't exist in the file, the operation fails with a clear
// error and no files are modified (atomic rollback).
func TestHandleCoreTool_BatchReplace_OldStrNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_batch_replace_3"

	// Create a file.
	originalContent := "line1\nline2\nline3\n"
	_, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "data.txt",
		"content": originalContent,
	}, tmpDir, taskID)
	if err != nil {
		t.Fatalf("setup write_file failed: %v", err)
	}

	// Try to replace text that doesn't exist.
	_, err = HandleCoreTool(context.Background(), "batch_replace", map[string]any{
		"replacements": []any{
			map[string]any{"file_path": "data.txt", "old_str": "nonexistent", "new_str": "whatever"},
		},
	}, tmpDir, taskID)
	if err == nil {
		t.Fatal("expected error for old_str not found, got nil")
	}

	// File should be unchanged (rollback).
	data, _ := os.ReadFile(filepath.Join(tmpDir, taskID, "data.txt"))
	if string(data) != originalContent {
		t.Errorf("file should be unchanged after failed replace, got: %s", string(data))
	}
}

// TestHandleCoreTool_BatchReplace_EmptyReplacements verifies the error
// when the replacements array is missing or empty.
func TestHandleCoreTool_BatchReplace_EmptyReplacements(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_batch_replace_4"

	_, err := HandleCoreTool(context.Background(), "batch_replace", map[string]any{
		"replacements": []any{},
	}, tmpDir, taskID)
	if err == nil {
		t.Fatal("expected error for empty replacements, got nil")
	}
	if !strContains(err.Error(), "required") && !strContains(err.Error(), "empty") {
		t.Errorf("error should mention required/empty, got: %s", err.Error())
	}
}

// ─── Defect 6: write_file verification hint (harness-bench §3.6) ───

// TestHandleCoreTool_WriteFileHint verifies that write_file includes a
// verification hint in its response, nudging the model to verify its
// output. Without this, the model writes files with bugs (e.g. missing
// JOIN in SQL) and never catches them because there's no "read back /
// run after write" closed loop.
func TestHandleCoreTool_WriteFileHint(t *testing.T) {
	tmpDir := t.TempDir()
	taskID := "test_hint"

	cases := []struct {
		path     string
		wantHint string
	}{
		{"solve.py", "verify"},
		{"query.sql", "verify"},
		{"run.sh", "verify"},
		{"data.json", "read_file"},
	}
	for _, tc := range cases {
		result, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
			"path":    tc.path,
			"content": "test content",
		}, tmpDir, taskID)
		if err != nil {
			t.Fatalf("write_file %s failed: %v", tc.path, err)
		}
		if !strContains(result, "Written") {
			t.Errorf("%s: missing 'Written' in result: %s", tc.path, result)
		}
		if !strContains(result, tc.wantHint) {
			t.Errorf("%s: missing hint %q in result: %s", tc.path, tc.wantHint, result)
		}
	}
}
