package core

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// dockerAvailable checks if docker is installed and running.
func dockerAvailable() bool {
	cmd := exec.Command("docker", "info", "--format", "{{.ServerVersion}}")
	return cmd.Run() == nil
}

func TestDockerProvider_HealthCheck(t *testing.T) {
	p := NewDockerProvider("outputs", "", "")
	err := p.HealthCheck(context.Background())
	if err != nil && !dockerAvailable() {
		t.Skip("docker not available, skipping")
	}
	if err != nil {
		t.Errorf("HealthCheck failed: %v", err)
	}
}

func TestDockerProvider_ExecutePython(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available, skipping")
	}

	p := NewDockerProvider("outputs", "python:3.12-slim", "none")
	result, err := p.Execute(context.Background(), ExecRequest{
		Script:   `print("hello from docker")`,
		Language: "python",
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello from docker") {
		t.Errorf("expected 'hello from docker' in stdout, got: %s", result.Stdout)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d (stderr: %s)", result.ExitCode, result.Stderr)
	}
}

func TestDockerProvider_UnsupportedLanguage(t *testing.T) {
	p := NewDockerProvider("outputs", "", "")
	_, err := p.Execute(context.Background(), ExecRequest{
		Script:   "whatever",
		Language: "brainfuck",
	})
	if err == nil {
		t.Error("expected error for unsupported language")
	}
}

func TestDockerProvider_Defaults(t *testing.T) {
	p := NewDockerProvider("outputs", "", "")
	if p.image != "python:3.12-slim" {
		t.Errorf("expected default image, got %s", p.image)
	}
	if p.network != "none" {
		t.Errorf("expected default network 'none', got %s", p.network)
	}
}

func TestDockerProvider_CloseSession(t *testing.T) {
	p := NewDockerProvider("outputs", "", "")
	if err := p.CloseSession("any"); err != nil {
		t.Errorf("CloseSession should be no-op: %v", err)
	}
}

func TestDockerProvider_ApplyExistingSandbox(t *testing.T) {
	p := NewDockerProvider("outputs", "", "")
	cleanup, err := p.ApplyExistingSandbox(12345, 512, 10)
	if err != nil {
		t.Errorf("ApplyExistingSandbox should not error: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
}
