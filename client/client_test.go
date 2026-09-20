package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMCPClientStderrCapture(t *testing.T) {
	tmpDir := t.TempDir()
	var cmd string
	var args []string
	var scriptPath string

	if runtime.GOOS == "windows" {
		cmd = "powershell"
		scriptPath = filepath.Join(tmpDir, "crash.ps1")
		scriptContent := `
[Console]::Error.Write("Simulated crash error message")
Start-Sleep -Milliseconds 100
exit 1
`
		if err := os.WriteFile(scriptPath, []byte(scriptContent), 0644); err != nil {
			t.Fatalf("failed to write script: %v", err)
		}
		args = []string{"-File", scriptPath}
	} else {
		// Use sh for Linux/Alpine
		cmd = "sh"
		scriptPath = filepath.Join(tmpDir, "crash.sh")
		scriptContent := `
echo "Simulated crash error message" >&2
sleep 0.1
exit 1
`
		if err := os.WriteFile(scriptPath, []byte(scriptContent), 0755); err != nil {
			t.Fatalf("failed to write script: %v", err)
		}
		args = []string{scriptPath}
	}

	// Verify the executable exists
	if _, err := exec.LookPath(cmd); err != nil {
		t.Skipf("skipping test: %s not found in PATH", cmd)
	}

	cli, err := NewMCPClient(cmd, args, "", nil, nil)
	if err != nil {
		t.Fatalf("failed to start client: %v", err)
	}
	defer cli.Close()

	// Wait for the script to crash
	time.Sleep(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = cli.SendRequest(ctx, "list_tools", map[string]any{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	t.Logf("Captured error: %v", err)

	expected := "Simulated crash error message"
	if !strings.Contains(err.Error(), expected) {
		t.Errorf("expected error to contain %q, got %q", expected, err.Error())
	}
}

func TestNewMCPClient_DirAndEnv(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "mcpclient_test_*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		cmd = "powershell"
		args = []string{"-Command", "echo hello"}
	} else {
		cmd = "sh"
		args = []string{"-c", "echo hello"}
	}

	env := map[string]string{"TEST_VAR": "magic-value"}

	cli, err := NewMCPClient(cmd, args, tmpDir, env, nil)
	if err != nil {
		t.Fatalf("NewMCPClient failed: %v", err)
	}
	defer cli.Close()

	if cli.cmd.Dir != tmpDir {
		t.Errorf("expected Dir %q, got %q", tmpDir, cli.cmd.Dir)
	}

	found := false
	for _, e := range cli.cmd.Env {
		if e == "TEST_VAR=magic-value" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected TEST_VAR=magic-value in Env, but not found")
	}
}
