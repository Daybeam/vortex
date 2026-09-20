package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/daybeam/vortex/client"
	"github.com/daybeam/vortex/pkg/env"
	"github.com/google/uuid"
)

// LocalProvider implements SandboxProvider using local OS resource limits (ADDED 2026-09-02).
type LocalProvider struct {
	outputBase string
}

func NewLocalProvider(outputBase string) *LocalProvider {
	return &LocalProvider{outputBase: outputBase}
}

// writeScript creates a temporary script file and returns its path plus the
// interpreter command and any interpreter-level args (e.g. ["run"] for bun).
// The caller is responsible for removing scriptPath.
func (p *LocalProvider) writeScript(req ExecRequest) (scriptPath, interpreter string, interpArgs []string, err error) {
	id := "sb_" + uuid.New().String()[:8]
	ext := ""

	switch req.Language {
	case "python":
		ext = ".py"
		interpreter = env.GetPythonCmd()
	case "node":
		ext = ".js"
		interpreter = "node"
	case "bun":
		ext = ".ts"
		interpreter = "bun"
		interpArgs = []string{"run"}
	case "lua":
		ext = ".lua"
		interpreter = "lua"
	default:
		return "", "", nil, fmt.Errorf("local-sandbox: unsupported language: %s", req.Language)
	}

	tempDir := filepath.Join(p.outputBase, "sandbox_tmp")
	os.MkdirAll(tempDir, 0755)
	scriptPath = filepath.Join(tempDir, id+ext)
	if err := os.WriteFile(scriptPath, []byte(req.Script), 0644); err != nil {
		return "", "", nil, fmt.Errorf("local-sandbox: failed to write script: %w", err)
	}
	return scriptPath, interpreter, interpArgs, nil
}

// runSandboxedCommand starts cmd with env vars and resource limits applied,
// waits for it to finish, and returns the captured result.
func runSandboxedCommand(ctx context.Context, cmd *exec.Cmd, req ExecRequest, startTime time.Time) (*ExecResult, error) {
	if len(req.EnvVars) > 0 {
		e := os.Environ()
		for k, v := range req.EnvVars {
			e = append(e, k+"="+v)
		}
		cmd.Env = e
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("local-sandbox: failed to start: %w", err)
	}

	if req.MemoryMB > 0 || req.Timeout > 0 {
		constraints := client.SandboxConstraints{
			MaxMemoryBytes: req.MemoryMB * 1024 * 1024,
			MaxCPUSeconds:  uint64(req.Timeout.Seconds()),
			KillOnClose:    true,
		}
		cleanup, err := client.ApplySandbox(cmd.Process.Pid, constraints)
		if err == nil {
			defer cleanup()
		} else {
			fmt.Fprintf(os.Stderr, "[local-sandbox] WARNING: failed to apply limits to pid=%d: %v\n", cmd.Process.Pid, err)
		}
	}

	err := cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Duration: time.Since(startTime),
	}, nil
}

func (p *LocalProvider) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	startTime := time.Now()

	scriptPath, interpreter, interpArgs, err := p.writeScript(req)
	if err != nil {
		return nil, err
	}
	defer os.Remove(scriptPath)

	cmdArgs := append(interpArgs, scriptPath)
	cmd := exec.CommandContext(ctx, interpreter, cmdArgs...)
	return runSandboxedCommand(ctx, cmd, req, startTime)
}

func (p *LocalProvider) CloseSession(sessionID string) error {
	// LocalProvider doesn't support long-lived REPL sessions in this phase
	return nil
}

func (p *LocalProvider) HealthCheck(ctx context.Context) error {
	return nil
}

func (p *LocalProvider) ApplyExistingSandbox(pid int, memoryMB uint64, cpuSecs uint64) (func(), error) {
	constraints := client.SandboxConstraints{
		MaxMemoryBytes: memoryMB * 1024 * 1024,
		MaxCPUSeconds:  cpuSecs,
		KillOnClose:    true,
	}
	return client.ApplySandbox(pid, constraints)
}
