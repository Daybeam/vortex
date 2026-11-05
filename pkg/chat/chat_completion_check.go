package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/core"
)

// Completion checks on the chat path.
//
// The chat path's only quality gate is `core.WorkspaceArtifactDefects`, and it
// stops at the type layer: "the file is there and parses as JSON", never "the
// file does what the task asked". 030 measured the gap — three of five v77 runs
// delivered SQL that cannot execute, and the contract passed all three with
// `contract_defects=0`.
//
// This file adds a second source of defects without adding a capability: the
// check body comes from a file the task declares (VORTEX_COMPLETION_CHECKS),
// and it runs through `execute_code`, i.e. the same external sandbox provider the
// model already uses. The engine's only new knowledge is "exit 0 = pass".
//
// Everything here is inert unless the directory is configured. That matters for
// the arms: with the variable unset, the run is byte-identical to today.
const completionCheckEnv = "VORTEX_COMPLETION_CHECKS"

// artifactDefects is the type-layer verdict plus any declared checks. Kept as a
// one-line call site inside Run so chat_harness.go does not grow (Q6-1 ratchet).
func (h *ChatHarness) artifactDefects(ctx context.Context, taskID string) []string {
	defects := core.WorkspaceArtifactDefects(h.SessionRoot, h.declaredFields)
	return append(defects, h.completionCheckDefects(ctx, taskID)...)
}

// completionCheckDefects runs each declared check and returns one line per
// failure, in the same shape as WorkspaceArtifactDefects so the existing steer
// channel and its cap apply unchanged.
func (h *ChatHarness) completionCheckDefects(ctx context.Context, taskID string) []string {
	dir := strings.TrimSpace(os.Getenv(completionCheckEnv))
	if dir == "" || h.SessionRoot == "" {
		return nil
	}
	checks := core.LoadCompletionChecks(dir, h.declaredArtifacts)
	if len(checks) == 0 {
		return nil
	}
	var out []string
	for _, c := range checks {
		// Resolve against the workspace: the engine's own cwd is the deploy
		// directory, so a bare os.Stat here would answer about the wrong tree.
		if _, err := os.Stat(filepath.Join(h.SessionRoot, c.Artifact)); err != nil {
			continue
		}
		raw, err := h.execTool(ctx, "execute_code", map[string]any{
			"code":     core.CompletionCheckProgram(c),
			"language": "python",
		}, taskID)
		if err != nil {
			// Harness fault (no runtime, sandbox down). Blaming the artefact for
			// it would send the model to fix work that is already correct.
			continue
		}
		var res struct {
			ExitCode *int   `json:"exit_code"`
			Stderr   string `json:"stderr"`
		}
		if json.Unmarshal([]byte(raw), &res) != nil || res.ExitCode == nil || *res.ExitCode == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s", c.Artifact, lastLine(res.Stderr)))
	}
	return out
}

// lastLine keeps the steer short: the check prints the failing object and the
// driver's message on its last stderr line, and everything before it is context
// the model does not need in a defect list.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			if r := []rune(t); len(r) > 160 {
				return string(r[:160]) + "..."
			}
			return t
		}
	}
	return "check failed"
}
