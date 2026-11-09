package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/env"
)

// ─── External Script Guard ─────────────────────────────────────────────────

// ExternalScriptGuard runs a user-supplied script that can block or allow
// commands before execution. The script receives JSON on stdin and signals
// its decision via exit code:
//
//	deny mode (default): exit 0 = allow, non-zero = block
//	allow mode:          exit 0 = allow, non-zero = block
//
// The difference between modes is the fail-closed behavior on timeout or
// execution error: deny mode blocks (the user asked for strictness), allow
// mode allows (the user asked for an allowlist, so absence of a decision
// means "not on the allowlist" → but we can't distinguish that from a
// broken script, so we allow to avoid false positives).
//
// On block, stdout is used as the block reason message.
type ExternalScriptGuard struct {
	name    string
	script  string
	timeout time.Duration
	mode    string // "deny" (default) or "allow"
	runtimes config.ExternalRuntimes
}

// NewExternalScriptGuard creates a guard from config.
func NewExternalScriptGuard(cfg config.CommandGuardConfig, runtimes config.ExternalRuntimes) *ExternalScriptGuard {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 500 * time.Millisecond
	}
	mode := cfg.Mode
	if mode == "" {
		mode = "deny"
	}
	return &ExternalScriptGuard{
		name:    cfg.Name,
		script:  cfg.Script,
		timeout: timeout,
		mode:    mode,
		runtimes: runtimes,
	}
}

func (g *ExternalScriptGuard) Name() string { return g.name }

// guardInput is the JSON payload sent to the guard script on stdin.
type guardInput struct {
	Command string `json:"command"`
	Args    []string `json:"args"`
	Body    string `json:"body"`
	Cwd     string `json:"cwd,omitempty"`
}

func (g *ExternalScriptGuard) Check(command string, args []string, body string) CommandGuardResult {
	input := guardInput{Command: command, Args: args, Body: body}
	inputBytes, _ := json.Marshal(input)

	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	exe, extraArgs, err := g.resolveInterpreter()
	if err != nil {
		// Can't determine how to run the script — fail-closed in deny mode.
		if g.mode == "deny" {
			return CommandGuardResult{Block: true, Message: fmt.Sprintf("guard %s: cannot resolve interpreter: %v", g.name, err)}
		}
		return CommandGuardResult{}
	}

	cmdArgs := append(append([]string{}, extraArgs...), g.script)
	cmd := exec.CommandContext(ctx, exe, cmdArgs...)
	cmd.Stdin = bytes.NewReader(inputBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Timeout or execution failure.
		if ctx.Err() == context.DeadlineExceeded {
			if g.mode == "deny" {
				return CommandGuardResult{Block: true, Message: fmt.Sprintf("guard %s: timeout after %v", g.name, g.timeout)}
			}
			return CommandGuardResult{}
		}
		// Non-zero exit code = block (both modes).
		reason := strings.TrimSpace(stdout.String())
		if reason == "" {
			reason = fmt.Sprintf("guard %s rejected (exit code non-zero)", g.name)
		}
		return CommandGuardResult{Block: true, Message: reason}
	}

	// Exit 0 = allow (both modes).
	return CommandGuardResult{}
}

// resolveInterpreter determines how to execute the guard script based on
// its file extension. Follows the same pattern as hook_runner.runSubprocess.
func (g *ExternalScriptGuard) resolveInterpreter() (exe string, extraArgs []string, err error) {
	ext := strings.ToLower(filepath.Ext(g.script))
	switch ext {
	case ".sh":
		return "sh", nil, nil
	case ".py":
		py := g.runtimes.PythonPath
		if py == "" {
			py = env.GetPythonCmd()
		}
		return py, nil, nil
	case ".js":
		nd := g.runtimes.NodePath
		if nd == "" {
			nd = "node"
		}
		return nd, nil, nil
	default:
		if cmdParts, ok := g.runtimes.Runtimes[ext]; ok && len(cmdParts) > 0 {
			return cmdParts[0], cmdParts[1:], nil
		}
		return "", nil, fmt.Errorf("unsupported guard script extension: %s", ext)
	}
}

// RegisterExternalGuards reads command guard configs and registers each
// as an ExternalScriptGuard. Called at startup after config is loaded.
func RegisterExternalGuards(configs []config.CommandGuardConfig, runtimes config.ExternalRuntimes) {
	for _, cfg := range configs {
		RegisterCommandGuard(NewExternalScriptGuard(cfg, runtimes))
	}
}
