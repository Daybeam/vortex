package client

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestKillProcessTreeByPID_InvalidPID verifies that invalid PIDs (0, negative)
// are no-ops and return nil without error (audit H2).
func TestKillProcessTreeByPID_InvalidPID(t *testing.T) {
	if err := KillProcessTreeByPID(0); err != nil {
		t.Fatalf("KillProcessTreeByPID(0) should be nil, got %v", err)
	}
	if err := KillProcessTreeByPID(-1); err != nil {
		t.Fatalf("KillProcessTreeByPID(-1) should be nil, got %v", err)
	}
	if err := KillProcessTreeByPID(-999); err != nil {
		t.Fatalf("KillProcessTreeByPID(-999) should be nil, got %v", err)
	}
}

// TestKillProcessTreeByPID_KillsProcess verifies that KillProcessTreeByPID
// actually terminates a running process. This is the regression test for H2:
// previously Close() only killed the parent process (Process.Kill), leaving
// child processes orphaned. KillProcessTreeByPID kills the entire tree by PID.
func TestKillProcessTreeByPID_KillsProcess(t *testing.T) {
	// Spawn a long-running process.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "timeout", "/t", "30", "/nobreak")
	} else {
		cmd = exec.Command("sleep", "30")
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start test process: %v", err)
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		t.Fatalf("invalid PID: %d", pid)
	}

	// Kill the process tree by PID.
	if err := KillProcessTreeByPID(pid); err != nil {
		// On some systems taskkill/pkill may report a non-nil error even
		// when the kill succeeds (e.g., "no such process" if the process
		// already exited). Only fail if the process is still running.
		t.Logf("KillProcessTreeByPID returned error: %v (may be benign)", err)
	}

	// Verify the process exits within a reasonable timeout.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// good — process was killed and reaped
	case <-time.After(5 * time.Second):
		// Force cleanup and fail.
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("process did not exit after KillProcessTreeByPID — kill failed (H2)")
	}
}

// TestKillProcessTree_EmptyPattern verifies that an empty pattern is a no-op
// (audit H2).
func TestKillProcessTree_EmptyPattern(t *testing.T) {
	if err := KillProcessTree(""); err != nil {
		t.Fatalf("KillProcessTree(\"\") should be nil, got %v", err)
	}
}
