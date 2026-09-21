//go:build darwin

package client

import (
	"fmt"
	"os"
)

type SandboxConstraints struct {
	MaxMemoryBytes uint64
	MaxCPUSeconds  uint64
	KillOnClose    bool
}

var DefaultSandboxConstraints = SandboxConstraints{
	MaxMemoryBytes: 256 * 1024 * 1024, // 256 MB
	MaxCPUSeconds:  30,
	KillOnClose:    true,
}

// ApplySandbox is not supported on macOS (no cgroups v2, no Job Objects).
// Logs a warning and returns a no-op cleanup — the tool will run unsandboxed.
func ApplySandbox(pid int, c SandboxConstraints) (cleanup func(), err error) {
	fmt.Fprintf(os.Stderr,
		"[jit-sandbox] NOT SUPPORTED on darwin: pid=%d — sandboxing disabled\n",
		pid)
	return func() {}, nil
}
