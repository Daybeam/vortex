package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
//
// emit is threaded through because a check that PASSES used to leave no trace
// anywhere, so "this run had no defects" read the same as "this run never ran a
// check". Two harness-bench runs proved it matters: 030's v78pair-off-r5/r9
// scored 0.0 with `contract_defects=0` and `defect_steer=0`, while their final
// SQL fails the very check the task declares
// (`helper_included_lines -> no such column: r.line_refund_total`). Nothing on
// disk could say whether the check had run at all.
func (h *ChatHarness) artifactDefects(ctx context.Context, taskID string, emit func(ChatEvent) error) []string {
	defects := core.WorkspaceArtifactDefects(h.SessionRoot, h.declaredFields)
	defects = append(defects, h.completionCheckDefects(ctx, taskID, emit)...)
	// Paths the workspace grew that the contract never allowed. Inert unless the
	// contract is modify-only -- newPathDefects self-gates (RESULT §28.7).
	return append(defects, h.newPathDefects()...)
}

// armedCompletionChecks resolves which checks this run could execute. Nil means
// the mechanism is off for this run (no directory, no workspace, or no check
// file matches a declared artifact) — it does NOT mean the artifacts are clean.
//
// The banner is logged HERE rather than at the call site that actually runs the
// checks, because that call site is the completion point: a run that dies before
// producing a complete artifact set (030's v79 smoke run: `exit=error`,
// `tool_calls=0`) would then print nothing, which is the same silence this file
// exists to remove. This function is reached on every turn by the observation
// line, so `armed=N` lands on disk for every run that gets far enough to have a
// summary at all.
func (h *ChatHarness) armedCompletionChecks() []core.CompletionCheck {
	var checks []core.CompletionCheck
	if dir := strings.TrimSpace(os.Getenv(completionCheckEnv)); dir != "" && h.SessionRoot != "" {
		checks = core.LoadCompletionChecks(dir, h.declaredArtifacts)
	}
	logCompletionChecksOnce(len(checks))
	return checks
}

// completionChecksLogged keeps the resolution visible once per process, in the
// same shape as LogCompletionContractOnce (→107: a value that is never printed
// is indistinguishable from "parsed and applied the default").
var completionChecksLogged sync.Once

func logCompletionChecksOnce(armed int) {
	completionChecksLogged.Do(func() {
		raw, present := os.LookupEnv(completionCheckEnv)
		source := "unset"
		if present {
			source = "env"
		}
		fmt.Fprintf(os.Stderr, "[config] %s=%q (source=%s) -> completion checks armed=%d\n",
			completionCheckEnv, raw, source, armed)
	})
}

// completionCheckDefects runs each declared check and returns one line per
// failure, in the same shape as WorkspaceArtifactDefects so the existing steer
// channel and its cap apply unchanged.
//
// One `completion_check` event is emitted per attempt, including the passes.
// The outcomes are distinct values rather than a bool because "the check failed"
// and "the check could not be run" need opposite responses: the first sends the
// model to fix the artifact, the second must not blame it for a sandbox that is
// down. The old code folded both into `continue`, i.e. into looking clean.
//
// Reading the payload takes care: `execute_code` deliberately OMITS exit_code
// when it succeeded (core/core_tools.go:500, "#17: omit exit_code/error so the
// model doesn't treat exit_code:0 as a domain success signal") and only sends
// `exit_code` + `error` on failure. So a missing exit_code is the pass shape, and
// treating it as "unknown" would report every good artifact as unverified.
func (h *ChatHarness) completionCheckDefects(ctx context.Context, taskID string, emit func(ChatEvent) error) []string {
	checks := h.armedCompletionChecks()
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
		started := time.Now()
		raw, err := h.execTool(ctx, "execute_code", map[string]any{
			"code":     core.CompletionCheckProgram(c),
			"language": "python",
		}, taskID)
		meta := map[string]any{
			"artifact":   c.Artifact,
			"elapsed_ms": time.Since(started).Milliseconds(),
		}

		var res struct {
			ExitCode *int   `json:"exit_code"`
			Stderr   string `json:"stderr"`
			Error    string `json:"error"`
		}
		unparsable := err == nil && json.Unmarshal([]byte(raw), &res) != nil

		switch {
		case err != nil:
			// Harness fault (no runtime, sandbox down). Blaming the artefact for
			// it would send the model to fix work that is already correct.
			meta["outcome"] = "harness_error"
		case unparsable:
			meta["outcome"] = "unparsable_result"
		case res.ExitCode != nil && *res.ExitCode == 0:
			meta["outcome"] = "pass"
			meta["exit_code"] = 0
		case res.ExitCode != nil:
			meta["outcome"] = "fail"
			meta["exit_code"] = *res.ExitCode
			line := lastLine(res.Stderr)
			meta["stderr_tail"] = line
			out = append(out, fmt.Sprintf("%s: %s", c.Artifact, line))
		case res.Error != "":
			// Failed, but the code did not come back. Still a defect: `error` is
			// only sent on the non-zero branch.
			meta["outcome"] = "fail"
			line := lastLine(res.Error)
			meta["stderr_tail"] = line
			out = append(out, fmt.Sprintf("%s: %s", c.Artifact, line))
		default:
			// No exit_code and no error is the documented success shape (#17).
			meta["outcome"] = "pass"
			meta["exit_code_omitted"] = true
		}
		if emit != nil {
			emit(ChatEvent{Type: EvCompletionCheck, Meta: meta})
		}
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
