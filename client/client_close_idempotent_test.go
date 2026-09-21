package client

import (
	"runtime"
	"testing"
)

// TestMCPClient_CloseIdempotent verifies that calling Close() multiple times
// is safe — no panic, no error. This is the regression test for audit M5:
// Close() was not idempotent, so a double-close would panic on stdin.Close()
// or error on cmd.Wait() (process already reaped).
func TestMCPClient_CloseIdempotent(t *testing.T) {
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "powershell"
		args = []string{"-Command", "Start-Sleep -Seconds 30"}
	} else {
		cmd = "sh"
		args = []string{"-c", "sleep 30"}
	}

	cli, err := NewMCPClient(cmd, args, "", nil, nil)
	if err != nil {
		t.Fatalf("NewMCPClient failed: %v", err)
	}

	// Close twice — must not panic.
	cli.Close()
	cli.Close() // should be a no-op, not a panic

	// Third call for good measure.
	cli.Close()
}
