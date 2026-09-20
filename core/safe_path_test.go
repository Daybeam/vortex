package core

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSafePathValidator_ResolveTaskDir(t *testing.T) {
	v := NewSafePathValidator("/tmp/task1", "", nil)
	got, err := v.Resolve("task", "output.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/tmp/task1", "output.txt")
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestSafePathValidator_ResolveSessionRoot(t *testing.T) {
	v := NewSafePathValidator("/tmp/task1", "/projects/myapp", nil)
	got, err := v.Resolve("session", "src/main.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/projects/myapp", "src/main.go")
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestSafePathValidator_ResolveAllowedRoot(t *testing.T) {
	v := NewSafePathValidator("/tmp/task1", "", []string{"/root/projects", "/tmp/eval"})
	got, err := v.Resolve("/root/projects", "input.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/root/projects", "input.txt")
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestSafePathValidator_RejectsTraversal(t *testing.T) {
	v := NewSafePathValidator("/tmp/task1", "", nil)
	cases := []string{"../etc/passwd", "../../secret", "/etc/passwd"}
	for _, rel := range cases {
		if _, err := v.Resolve("task", rel); err == nil {
			t.Errorf("expected error for traversal path %q, got nil", rel)
		}
	}
}

func TestSafePathValidator_DefaultBaseIsTask(t *testing.T) {
	v := NewSafePathValidator("/tmp/task1", "", nil)
	got, err := v.Resolve("", "file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join("/tmp/task1", "file.txt")
	if got != want {
		t.Errorf("Resolve with empty base = %q, want %q", got, want)
	}
}

func TestSafePathValidator_UnknownBaseReturnsError(t *testing.T) {
	v := NewSafePathValidator("/tmp/task1", "", nil)
	if _, err := v.Resolve("nonexistent", "file.txt"); err == nil {
		t.Error("expected error for unknown base")
	}
}

func TestSafePathValidator_IsAllowed(t *testing.T) {
	v := NewSafePathValidator("/tmp/task1", "/projects/app", []string{"/data"})
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/task1/output.txt", true},
		{"/projects/app/src/main.go", true},
		{"/data/input.csv", true},
		{"/etc/passwd", false},
	}
	for _, c := range cases {
		if got := v.IsAllowed(c.path); got != c.want {
			t.Errorf("IsAllowed(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestSafePathValidator_BackwardCompatible_EmptySessionAndRoots(t *testing.T) {
	v := NewSafePathValidator("/tmp/task1", "", nil)
	got, err := v.Resolve("task", "report.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	legacy, legacyErr := safeJoin("/tmp/task1", "report.json")
	if legacyErr != nil {
		t.Fatalf("safeJoin error: %v", legacyErr)
	}
	if got != legacy {
		t.Errorf("validator.Resolve = %q, safeJoin = %q; should be identical", got, legacy)
	}
}

func TestToolFailFeedback_BelowThreshold(t *testing.T) {
	got := toolFailFeedback("exec_command", "timeout", 1)
	if got == "" {
		t.Error("expected non-empty feedback")
	}
}

func TestToolFailFeedback_AtThreshold_CircuitBreaker(t *testing.T) {
	got := toolFailFeedback("exec_command", "timeout", 3)
	if got == "" {
		t.Error("expected non-empty feedback")
	}
}

func TestToolFailFeedback_AboveThreshold_CircuitBreaker(t *testing.T) {
	got := toolFailFeedback("exec_command", "timeout", 5)
	if got == "" {
		t.Error("expected non-empty feedback")
	}
}

func TestHandleCoreTool_WithValidator(t *testing.T) {
	tmp := t.TempDir()
	v := NewSafePathValidator(filepath.Join(tmp, "task1"), "", nil)
	_, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "test.txt",
		"content": "hello",
	}, tmp, "task1", v)
	if err != nil {
		t.Fatalf("write_file with validator: %v", err)
	}

	result, err := HandleCoreTool(context.Background(), "read_file", map[string]any{
		"path": "test.txt",
	}, tmp, "task1", v)
	if err != nil {
		t.Fatalf("read_file with validator: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty read result")
	}
}

func TestHandleCoreTool_WithValidator_RejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	v := NewSafePathValidator(filepath.Join(tmp, "task1"), "", nil)
	_, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "../escape.txt",
		"content": "evil",
	}, tmp, "task1", v)
	if err == nil {
		t.Error("expected error for path traversal with validator")
	}
}
