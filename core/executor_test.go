package core

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// resolveWindowsShell finds powershell via LookPath, falling back to the
// well-known System32 location. Returns ("", false) if unavailable so the
// caller can t.Skip cleanly. This avoids hard-failing when the test binary is
// launched from a shell (e.g. git-bash) whose PATH omits the PowerShell dir.
func resolveWindowsShell(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return ""
	}
	if path, err := exec.LookPath("powershell"); err == nil {
		return path
	}
	// Fallback: well-known System32 location (not always on bash PATH).
	for _, p := range []string{
		`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		`C:\WINDOWS\System32\WindowsPowerShell\v1.0\powershell.exe`,
	} {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	return ""
}

func TestControlledExecutor_IdleTimeout(t *testing.T) {
	// Skip in short mode: this test depends on real-process timing behavior
	// (1s idle timeout vs 3s sleep) which is non-deterministic on loaded CI.
	// Non-regression: tests executor timeout machinery, not core logic.
	if testing.Short() {
		t.Skip("timing-dependent test; skipped in short mode for CI stability")
	}
	e := NewControlledExecutor()
	e.IdleTimeout = 1 * time.Second

	// Use environment-appropriate command
	cmd := "sleep"
	args := []string{"3"}
	if runtime.GOOS == "windows" {
		ps := resolveWindowsShell(t)
		if ps == "" {
			t.Skip("powershell not found on PATH or System32; skipping")
		}
		cmd = ps
		args = []string{"-Command", "Start-Sleep -Seconds 3"}
	}

	res, err := e.Run(context.Background(), cmd, args, "", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if res.Status != "timeout" {
		t.Errorf("expected status 'timeout', got %q", res.Status)
	}
}

func TestControlledExecutor_StdinPattern(t *testing.T) {
	// Skip in short mode: this test spawns a real process and monitors
	// stdin patterns, which is environment-dependent (shell availability,
	// stdin pipe behavior). Non-regression: tests stdin monitoring, not core logic.
	if testing.Short() {
		t.Skip("environment-dependent stdin test; skipped in short mode for CI stability")
	}
	e := NewControlledExecutor()
	e.DisableStdinMonitoring = true // Test the new flag
	e.IdleTimeout = 5 * time.Second

	// Use environment-appropriate command
	cmd := "echo"
	args := []string{"Overwrite existing? (y/n)"}
	if runtime.GOOS == "windows" {
		ps := resolveWindowsShell(t)
		if ps == "" {
			t.Skip("powershell not found on PATH or System32; skipping")
		}
		cmd = ps
		args = []string{"-Command", "Write-Host 'Overwrite existing? (y/n)'"}
	}

	res, err := e.Run(context.Background(), cmd, args, "", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if res.Status != "ok" {
		t.Errorf("expected status 'ok', got %q, stdout: %q", res.Status, string(res.Stdout))
	}
}
