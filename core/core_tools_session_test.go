package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandleCoreTool_SessionRootRead verifies that read_file resolves
// relative paths against SessionRoot when the file exists there.
func TestHandleCoreTool_SessionRootRead(t *testing.T) {
	sessionDir := t.TempDir()
	taskDir := t.TempDir()

	// Create a file inside SessionRoot.
	subdir := filepath.Join(sessionDir, "in")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "alpha\nbeta\ngamma\ndelta"
	if err := os.WriteFile(filepath.Join(subdir, "input.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	validator := NewSafePathValidator(taskDir, sessionDir, nil)
	result, err := HandleCoreTool(context.Background(), "read_file", map[string]any{"path": "in/input.txt"}, taskDir, "test-task", validator)
	if err != nil {
		t.Fatalf("HandleCoreTool read_file failed: %v", err)
	}
	if result != content {
		t.Fatalf("expected %q, got %q", content, result)
	}
}

// TestHandleCoreTool_SessionRootWrite verifies that write_file writes to
// SessionRoot when available.
func TestHandleCoreTool_SessionRootWrite(t *testing.T) {
	sessionDir := t.TempDir()
	taskDir := t.TempDir()

	validator := NewSafePathValidator(taskDir, sessionDir, nil)
	_, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "out/linecount.txt",
		"content": "4",
	}, taskDir, "test-task", validator)
	if err != nil {
		t.Fatalf("HandleCoreTool write_file failed: %v", err)
	}

	// Verify the file was written to SessionRoot.
	got, err := os.ReadFile(filepath.Join(sessionDir, "out", "linecount.txt"))
	if err != nil {
		t.Fatalf("file not found at SessionRoot: %v", err)
	}
	if string(got) != "4" {
		t.Fatalf("expected '4', got %q", string(got))
	}
}

// TestHandleCoreTool_TaskDirFallback verifies backward compatibility:
// when SessionRoot is empty, files resolve to taskDir.
func TestHandleCoreTool_TaskDirFallback(t *testing.T) {
	taskDir := t.TempDir()

	// Create a file in taskDir.
	subdir := filepath.Join(taskDir, "in")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	content := "hello world"
	if err := os.WriteFile(filepath.Join(subdir, "input.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	validator := NewSafePathValidator(taskDir, "", nil)
	result, err := HandleCoreTool(context.Background(), "read_file", map[string]any{"path": "in/input.txt"}, taskDir, "test-task", validator)
	if err != nil {
		t.Fatalf("HandleCoreTool read_file failed: %v", err)
	}
	if result != content {
		t.Fatalf("expected %q, got %q", content, result)
	}
}

// TestHandleCoreTool_ExecuteCodeCwd verifies that execute_code runs with
// SessionRoot as working directory.
func TestHandleCoreTool_ExecuteCodeCwd(t *testing.T) {
	sessionDir := t.TempDir()
	taskDir := t.TempDir()

	// Create a file in SessionRoot.
	subdir := filepath.Join(sessionDir, "data")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "test.txt"), []byte("line1\nline2\nline3"), 0644); err != nil {
		t.Fatal(err)
	}

	validator := NewSafePathValidator(taskDir, sessionDir, nil)
	result, err := HandleCoreTool(context.Background(), "execute_code", map[string]any{
		"code":     "import os; print(os.getcwd()); print(open('data/test.txt').read())",
		"language": "python",
	}, taskDir, "test-task", validator)
	if err != nil {
		t.Fatalf("HandleCoreTool execute_code failed: %v", err)
	}

	// The stdout should contain the sessionDir path and the file content.
	// result is JSON-encoded, so on Windows the cwd's backslashes are escaped
	// (C:\x -> C:\\x); escape the needle the same way to match.
	if !stringContains(result, strings.ReplaceAll(sessionDir, `\`, `\\`)) {
		t.Errorf("expected cwd to contain %s, got result: %s", sessionDir, result)
	}
	if !stringContains(result, "line1") {
		t.Errorf("expected file content in result, got: %s", result)
	}
}

func stringContains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && stringContainsSubstr(s, substr))
}

func stringContainsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
