package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DockerProvider implements SandboxProvider using Docker containers for
// isolation. Each Execute call runs a script in a ephemeral container.
//
// Design: PONYTAIL LADDER — uses `docker` CLI via os/exec (stdlib), no
// Docker SDK dependency. Containers are --rm (auto-removed on exit).
// Network is disabled by default (--network none) for security.
//
// Config (SandboxConfig):
//   - DockerImage: base image (default: python:3.12-slim)
//   - DockerNetwork: "none" (default), "host", or custom network name
type DockerProvider struct {
	outputBase string
	image      string
	network    string
}

func NewDockerProvider(outputBase, image, network string) *DockerProvider {
	if image == "" {
		image = "python:3.12-slim"
	}
	if network == "" {
		network = "none"
	}
	return &DockerProvider{
		outputBase: outputBase,
		image:      image,
		network:    network,
	}
}

// dockerLangConfig returns the file extension, interpreter, and base image
// override (if any) for a given language.
func dockerLangConfig(lang string) (ext, interpreter string, ok bool) {
	switch lang {
	case "python":
		return ".py", "python3", true
	case "node":
		return ".js", "node", true
	case "bun":
		return ".ts", "bun", true
	case "lua":
		return ".lua", "lua", true
	default:
		return "", "", false
	}
}

func (p *DockerProvider) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	startTime := time.Now()

	ext, interpreter, ok := dockerLangConfig(req.Language)
	if !ok {
		return nil, fmt.Errorf("docker-sandbox: unsupported language: %s", req.Language)
	}

	// Write script to a temp file on the host, mount it read-only into the container.
	id := "ds_" + uuid.New().String()[:8]
	tempDir := filepath.Join(p.outputBase, "sandbox_tmp")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("docker-sandbox: mkdir: %w", err)
	}
	scriptPath := filepath.Join(tempDir, id+ext)
	if err := os.WriteFile(scriptPath, []byte(req.Script), 0644); err != nil {
		return nil, fmt.Errorf("docker-sandbox: write script: %w", err)
	}
	defer os.Remove(scriptPath)

	// Docker on Windows requires forward-slash paths or the /host_mnt prefix.
	// Convert to absolute path and normalize separators.
	absScriptPath, _ := filepath.Abs(scriptPath)
	mountPath := filepath.ToSlash(absScriptPath)
	if runtime.GOOS == "windows" {
		// Docker Desktop on Windows expects /host_mnt/c:/path format for
		// local volume mounts from the host. However, the simpler approach
		// is to use the //c/path format which works in most Docker Desktop
		// versions. Use the drive letter without colon.
		// e.g. C:\Users\... → //c/Users/...
		if len(mountPath) >= 2 && mountPath[1] == ':' {
			drive := strings.ToLower(string(mountPath[0]))
			mountPath = "//" + drive + mountPath[2:]
		}
	}

	// Build docker run command.
	//   docker run --rm --network none -v <host>:/tmp/script:ro <image> <interpreter> /tmp/script
	containerScriptPath := "/tmp/script" + ext
	args := []string{
		"run", "--rm",
		"--name", id,
		"--network", p.network,
		"-v", mountPath + ":" + containerScriptPath + ":ro",
	}

	// Resource limits via --memory and --cpus.
	if req.MemoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", req.MemoryMB))
	}
	if req.Timeout > 0 {
		// Docker doesn't have a direct timeout flag, but we use the context
		// deadline (set below) to kill the container.
	}

	// Environment variables.
	for k, v := range req.EnvVars {
		args = append(args, "-e", k+"="+v)
	}

	args = append(args, p.image, interpreter, containerScriptPath)

	// Apply timeout via context.
	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	} else if _, ok := ctx.Deadline(); !ok {
		// audit P-MED-6: default 60s timeout when neither req.Timeout nor
		// caller ctx has a deadline — prevents infinite-loop scripts from
		// running forever.
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			// audit L-HIGH-2: non-ExitError (docker not installed, context
			// cancelled before start, I/O error) must NOT be silently masked.
			exitCode = -1
			result := &ExecResult{
				Stdout:   stdout.String(),
				Stderr:   stderr.String() + "\n[docker-sandbox] infrastructure error: " + err.Error(),
				ExitCode: exitCode,
				Duration: time.Since(startTime),
			}
			return result, err
		}
	}

	result := &ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		Duration: time.Since(startTime),
	}

	// audit L-MED-4: check any context error (DeadlineExceeded OR Canceled),
	// not just DeadlineExceeded — context.Canceled also means the run was
	// interrupted and exit code is unreliable.
	if runCtx.Err() != nil {
		result.Stderr = strings.TrimSpace(result.Stderr) + "\n[docker-sandbox] context error: " + runCtx.Err().Error()
		result.ExitCode = -1
		// audit R-MED-2: when context is cancelled/timed out, the docker
		// CLI process is killed but the container may still be running in
		// the daemon. Explicitly stop it so --rm can auto-remove it.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		stopCmd := exec.CommandContext(stopCtx, "docker", "stop", id)
		stopCmd.Run() // best-effort; ignore errors
	}

	return result, nil
}

func (p *DockerProvider) CloseSession(sessionID string) error {
	// DockerProvider uses --rm containers (ephemeral), no persistent sessions.
	return nil
}

func (p *DockerProvider) HealthCheck(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker-sandbox: docker not available: %w", err)
	}
	return nil
}

// ApplyExistingSandbox is not supported for Docker (containers are isolated
// by design). Falls back to no-op for interface compliance.
func (p *DockerProvider) ApplyExistingSandbox(pid int, memoryMB uint64, cpuSecs uint64) (func(), error) {
	// Docker containers already have resource limits set at creation time.
	// For host-level processes, this is a no-op.
	return func() {}, nil
}
