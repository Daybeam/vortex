package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/daybeam/vortex/schemas"
)

// DeterministicVerifier defines the zero-token hard verification contract.
// Implementations must be fast (milliseconds), side-effect-free (read-only),
// and never call an LLM. See docs/TIERED_SHORT_CIRCUIT_AUDIT.md
type DeterministicVerifier interface {
	Verify(ctx context.Context, workdir string, criteria map[string]any) (bool, string, error)
}

// deterministicVerifiers is the registry of all hard-check verifiers.
// Keyed by check.Type. Extend by adding new implementations before init.
var deterministicVerifiers = map[string]DeterministicVerifier{
	"file_exists":  &FileExistsVerifier{},
	"command_pass": &CommandPassVerifier{},
}

// FileExistsVerifier checks that a file exists and is non-empty.
// Params: { "path": "<relative or absolute path>" }
type FileExistsVerifier struct{}

func (v *FileExistsVerifier) Verify(ctx context.Context, workdir string, criteria map[string]any) (bool, string, error) {
	path, _ := criteria["path"].(string)
	if path == "" {
		return false, "missing 'path' parameter", nil
	}
	absPath := path
	if !filepath.IsAbs(absPath) && workdir != "" {
		absPath = filepath.Join(workdir, path)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return false, fmt.Sprintf("file not found: %s", path), nil
	}
	if info.Size() == 0 {
		return false, fmt.Sprintf("file is empty: %s", path), nil
	}
	return true, "", nil
}

// CommandPassVerifier checks that a shell command exits with code 0.
// Params: { "command": "<shell command>" }
// Uses sh -c on Unix, cmd /c on Windows.
type CommandPassVerifier struct{}

func (v *CommandPassVerifier) Verify(ctx context.Context, workdir string, criteria map[string]any) (bool, string, error) {
	cmdStr, _ := criteria["command"].(string)
	if cmdStr == "" {
		return false, "missing 'command' parameter", nil
	}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", cmdStr)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
	if workdir != "" {
		cmd.Dir = workdir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("command failed: %s\noutput: %s", err, string(output)), nil
	}
	return true, "", nil
}

// runDeterministicChecks executes all structured deterministic checks for a step.
// Returns (passed, failureMessage, failureType). If all pass, returns (true, "", "").
// workdir is the base directory for relative path resolution (typically s.outputBase).
func runDeterministicChecks(ctx context.Context, workdir string, checks []schemas.DeterministicCheck) (bool, string, string) {
	for _, check := range checks {
		verifier, ok := deterministicVerifiers[check.Type]
		if !ok {
			continue
		}
		passed, msg, err := verifier.Verify(ctx, workdir, check.Params)
		if err != nil || !passed {
			failType := "logic_error"
			if check.Type == "file_exists" {
				failType = "schema_violation"
			}
			return false, fmt.Sprintf("deterministic check [%s] failed: %s", check.Type, msg), failType
		}
	}
	return true, "", ""
}
