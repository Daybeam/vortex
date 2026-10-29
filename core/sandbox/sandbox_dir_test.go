package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── Defect 5: ExecRequest.Dir is honored by providers (report §3) ───

// TestExecRequest_HasDirField is a compile-time check that ExecRequest
// has a Dir field. (harness-bench report §3 defect 5, 2026-10-28)
func TestExecRequest_HasDirField(t *testing.T) {
	req := ExecRequest{Dir: "/some/path"}
	if req.Dir != "/some/path" {
		t.Error("ExecRequest.Dir field not set correctly")
	}
}

// TestLocalProvider_Execute_SetsWorkingDir verifies that LocalProvider.Execute
// runs the script with req.Dir as the working directory, not the engine
// process's cwd.
//
// Regression: LocalProvider.Execute never set cmd.Dir, so execute_code
// ran in the engine process cwd instead of the workspace root.
// (harness-bench report §3 defect 5, 2026-10-28)
func TestLocalProvider_Execute_SetsWorkingDir(t *testing.T) {
	workDir := t.TempDir()
	// Create a marker file in the working directory.
	markerPath := filepath.Join(workDir, "marker.txt")
	if err := os.WriteFile(markerPath, []byte("found"), 0644); err != nil {
		t.Fatal(err)
	}

	p := NewLocalProvider(t.TempDir())
	result, err := p.Execute(context.Background(), ExecRequest{
		Script:   "import os; print(os.path.exists('marker.txt'))",
		Language: "python",
		Timeout:  10 * time.Second,
		Dir:      workDir,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	// The script checks if marker.txt exists in the cwd. If cmd.Dir is set
	// correctly, it should find the file and print "True".
	if !strings.Contains(result.Stdout, "True") {
		t.Errorf("expected 'True' in stdout (marker.txt found in workDir), got stdout: %s", result.Stdout)
	}
}

// TestLocalProvider_Execute_EmptyDir_BackwardCompat verifies that when
// Dir is empty, the provider uses the default cwd (backward compatible).
func TestLocalProvider_Execute_EmptyDir_BackwardCompat(t *testing.T) {
	p := NewLocalProvider(t.TempDir())
	// This should work without setting Dir — uses process cwd.
	result, err := p.Execute(context.Background(), ExecRequest{
		Script:   "print('hello')",
		Language: "python",
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello") {
		t.Errorf("expected 'hello' in stdout, got: %s", result.Stdout)
	}
}
