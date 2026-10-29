package sandbox

import (
	"context"
	"time"
)

// ExecRequest defines the input for a sandbox execution (ADDED 2026-09-02).
type ExecRequest struct {
	Script    string // code to execute
	Language  string // python | node | lua | bun
	SessionID string // empty = one-shot; non-empty = long-lived REPL
	Timeout   time.Duration
	MemoryMB  uint64
	EnvVars   map[string]string
	// Dir sets the working directory for the executed code. When empty,
	// the provider's default cwd is used (backward compatible).
	// (ADDED 2026-10-28: harness-bench report §3 defect 5 — previously
	// the sandbox branch silently dropped cwd, so execute_code ran in the
	// engine process cwd instead of the workspace root.)
	Dir string
}

// ExecResult defines the output of a sandbox execution (ADDED 2026-09-02).
type ExecResult struct {
	SessionID string // echoed back for REPL sessions
	Stdout    string
	Stderr    string
	ExitCode  int
	Duration  time.Duration
}

// SandboxProvider is an abstraction over different sandbox backends (ADDED 2026-09-02).
type SandboxProvider interface {
	// Execute runs a script in an isolated environment and returns stdout/stderr.
	// For long-lived sessions (REPL), sessionID enables multi-turn interaction.
	Execute(ctx context.Context, req ExecRequest) (*ExecResult, error)

	// CloseSession terminates a long-lived sandbox (REPL/kernel).
	CloseSession(sessionID string) error

	// HealthCheck verifies the backend is reachable.
	HealthCheck(ctx context.Context) error

	// ApplyExistingSandbox enforces resource limits on an already-spawned local process.
	// This maintains compatibility with Tier 1 (local cgroups/Job Objects) for MCP servers.
	ApplyExistingSandbox(pid int, memoryMB uint64, cpuSecs uint64) (func(), error)
}
