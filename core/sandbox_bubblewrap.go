package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// BubblewrapProvider executes JIT scripts inside a bwrap user-namespace sandbox.
// It delegates script preparation, resource limits, and session lifecycle to
// LocalProvider, only wrapping the interpreter invocation with bwrap arguments.
// On non-Linux hosts or when bwrap is absent it transparently falls back to
// local execution, satisfying the Graceful Fallback design principle.
type BubblewrapProvider struct {
	*LocalProvider
	enabled   bool
	bwrapPath string
}

// NewBubblewrapProvider returns a BubblewrapProvider. On Linux it probes for
// the bwrap binary; everywhere else (or if not found) it stays disabled and
// logs a one-time fallback notice.
func NewBubblewrapProvider(outputBase string) *BubblewrapProvider {
	local := NewLocalProvider(outputBase)
	path, err := exec.LookPath("bwrap")
	enabled := runtime.GOOS == "linux" && err == nil
	if !enabled && runtime.GOOS == "linux" {
		fmt.Fprintf(os.Stderr, "[bubblewrap] bwrap not found on PATH; falling back to local execution\n")
	}
	return &BubblewrapProvider{
		LocalProvider: local,
		enabled:       enabled,
		bwrapPath:     path,
	}
}

// Execute runs the script under bwrap when enabled, otherwise delegates to
// LocalProvider. The sandbox isolates PID/IPC/UTS namespaces, mounts system
// dirs read-only, and only grants write access to the script's working dir.
func (p *BubblewrapProvider) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if !p.enabled {
		return p.LocalProvider.Execute(ctx, req)
	}

	startTime := time.Now()
	scriptPath, interpreter, interpArgs, err := p.writeScript(req)
	if err != nil {
		return nil, err
	}
	defer os.Remove(scriptPath)

	workDir := filepath.Dir(scriptPath)
	bwrapArgs := []string{
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf",
		"--proc", "/proc",
		"--dev", "/dev",
		"--bind", workDir, workDir,
		"--chdir", workDir,
		interpreter,
	}
	bwrapArgs = append(bwrapArgs, interpArgs...)
	bwrapArgs = append(bwrapArgs, scriptPath)

	cmd := exec.CommandContext(ctx, p.bwrapPath, bwrapArgs...)
	return runSandboxedCommand(ctx, cmd, req, startTime)
}
