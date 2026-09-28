package core

import (
	"context"
	"testing"
)

func TestControlledExecutor_Sandboxed_RunsWithoutError(t *testing.T) {
	exec := NewControlledExecutor()
	exec.Sandboxed = true
	exec.TotalTimeout = 10 * 1000 * 1000 * 1000 // 10s

	// Run a trivial command with sandbox enabled. On Windows this applies
	// a Job Object with 256MB/30s limits; on Linux cgroups; on macOS noop.
	// The command should succeed — the sandbox only limits resources, not
	// filesystem access.
	res, err := exec.Run(context.Background(), "go", []string{"version"}, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("expected status 'ok', got %q (stderr: %s)", res.Status, res.Stderr)
	}
}
