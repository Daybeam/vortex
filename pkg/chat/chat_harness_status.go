package chat

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// formatTaskStatus renders the orchestrator status dict for a model to read.
// Only the fields that change a decision are included: the terminal status, the
// per-step outcome (so a skip is visible), why it failed, and what it produced.
//
// "What it produced" is the part that was missing (harness-bench D5, 2026-10-30).
// The delegated agent writes into the workspace with write_file / execute_code,
// and those writes never reach graph.OutputFiles — that list only gains entries
// when a step declares an output_ref or a large JSON result is side-loaded. So a
// finished task came back as "status: completed / step s1: ok" with no hint that
// the deliverable was sitting in the workspace, and the only action available to
// the model was to ask again: one trial called this tool 34 times in 40 turns.
// Telling it where the files are, and that polling again cannot help, is what
// turns a status tool into a hand-off.
// declared is the task's artifact list parsed from the prompt's "Enforced
// workspace change contract" section. It is the only source of "which files this
// task was supposed to leave behind": the delegate contract has no outputs
// field, so nothing the subagent reports can fill that gap (→170).
func formatTaskStatus(id string, st map[string]any, declared []string) string {
	var b strings.Builder
	b.WriteString("task_id: " + id + "\n")
	status := ""
	if s, _ := st["status"].(string); s != "" {
		status = s
		b.WriteString("status: " + s + "\n")
	}
	if task, _ := st["task"].(string); task != "" {
		b.WriteString("task: " + task + "\n")
	}
	root, _ := st["workspace_root"].(string)
	if root != "" {
		b.WriteString("workspace_root: " + root + "\n")
	}
	// Progress summary: computed from the full view's steps map, because the
	// full view (which DelegateStatus returns) does not carry the summary
	// view's top-level "progress" or "active_step_id" fields. Without this,
	// a model polling a running task sees per-step statuses but no concise
	// "how far along" signal, so it cannot distinguish early progress from
	// near-completion (harness-bench D1, 2026-11-08).
	if steps, ok := st["steps"].(map[string]any); ok && len(steps) > 0 {
		completed := 0
		var activeStepID string
		for _, sv := range steps {
			sm, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			s, _ := sm["status"].(string)
			switch s {
			case "ok", "partial", "skipped":
				completed++
			case "running", "blocked":
				activeStepID = sm["id"].(string)
			}
		}
		b.WriteString(fmt.Sprintf("progress: %d/%d", completed, len(steps)))
		if activeStepID != "" {
			b.WriteString(" active_step: " + activeStepID)
		}
		b.WriteString("\n")
	}
	// Elapsed time from task creation. Gives the model a sense of whether the
	// task just started or has been running long enough to expect completion.
	if created, _ := st["created_at"].(string); created != "" {
		if t, err := time.Parse(time.RFC3339, created); err == nil {
			elapsed := time.Since(t)
			b.WriteString("elapsed: " + formatDuration(elapsed) + "\n")
		}
	}
	if steps, ok := st["steps"].(map[string]any); ok && len(steps) > 0 {
		ids := make([]string, 0, len(steps))
		for sid := range steps {
			ids = append(ids, sid)
		}
		sort.Strings(ids) // map iteration is unordered; a stable list is diffable
		for _, sid := range ids {
			sm, ok := steps[sid].(map[string]any)
			if !ok {
				continue
			}
			line := "step " + sid + ": "
			if s, _ := sm["status"].(string); s != "" {
				line += s
			} else {
				line += "unknown"
			}
			if e, _ := sm["last_error"].(string); e != "" {
				line += " (error: " + e + ")"
			}
			if r, _ := sm["result_ref"].(string); r != "" {
				line += " (result: " + r + ")"
			}
			b.WriteString(line + "\n")
		}
	}
	names := make([]string, 0)
	if files, ok := st["output_files"].([]map[string]any); ok && len(files) > 0 {
		for _, f := range files {
			p, _ := f["path"].(string)
			if p == "" {
				continue
			}
			// Declared outputs are recorded exactly as the step produced them,
			// which is usually relative. A relative path is useless to a model
			// that is about to write its own file: it has no idea what the
			// task's cwd was.
			if root != "" && !filepath.IsAbs(p) {
				p = filepath.Join(root, p)
			}
			names = append(names, p)
		}
		if len(names) > 0 {
			b.WriteString("output_files: " + strings.Join(names, ", ") + "\n")
		}
	}
	if decs, ok := st["pending_decisions"].([]map[string]any); ok && len(decs) > 0 {
		b.WriteString(fmt.Sprintf(
			"waiting on %d decision(s) from the caller — see the decision_needed event in this session\n",
			len(decs)))
	}
	// Terminal states get an explicit next action. Without it the tool answered
	// every poll with the same four lines, which reads like "still working" to a
	// model that has run out of ideas, and the only move left is to ask again.
	if isTerminalChatStatus(status) {
		switch {
		case len(names) > 0:
			b.WriteString("this task is finished — read the output_files above and answer. " +
				"Calling orchestrator_task_status again will not change anything.\n")
		case len(declared) > 0 && root != "":
			// The declared list comes from the task prompt's "Enforced workspace
			// change contract" section (h.declaredArtifacts), not from the
			// subagent: the delegate contract has no field for outputs, so
			// `names` above is always empty for a delegated task and this line
			// used to say nothing at all about the files. Measured on 011
			// b3d-v71-r5: the receipt said "declared no output files, go ls",
			// the parent could not tell "missing" from "elsewhere", and it spent
			// 32 execute_code calls rebuilding before the budget ended the run.
			//
			// These are counts, not a verdict: the list is
			// mutation_contract.allowed_created, which differs from the task's
			// required_artifacts on 185 of 274 tasks, so `present < declared`
			// must never be turned into "the task failed" (→171).
			absent := core.MissingArtifacts(root, declared)
			b.WriteString(fmt.Sprintf("declared %d artifact(s); present %d",
				len(declared), len(declared)-len(absent)))
			if len(absent) > 0 {
				b.WriteString("; missing: " + strings.Join(absent, ", "))
			}
			b.WriteString("\n")
			if len(absent) > 0 {
				b.WriteString("the missing ones are not under " + root + " by those names — list it " +
					"(e.g. execute_code with `ls -la`) before rebuilding anything, because a file " +
					"can exist in the wrong place. Do not poll this tool again for this task.\n")
			} else {
				b.WriteString("all declared artifacts exist under " + root +
					" — read them and answer. Do not poll this tool again for this task.\n")
			}
		case root != "":
			b.WriteString("this task is finished and declared no output files: the agents write " +
				"into the workspace, so list " + root + " (e.g. execute_code with `ls -la`) " +
				"to find the deliverable. Do not poll this tool again for this task. " +
				"no artifacts were declared for this task; if you expected files, the delegate " +
				"call must declare them.\n")
		default:
			b.WriteString("this task is finished and declared no output files. " +
				"Do not poll this tool again for this task.\n")
		}
	}
	return b.String()
}

// isTerminalChatStatus reports whether a task status string means "no further
// work will happen". Anything unrecognised is treated as non-terminal on
// purpose: the cost of one extra poll is a wasted turn, the cost of calling a
// running task finished is a wrong answer. The constants are used rather than
// literals so a renamed GraphStatus fails to compile here instead of silently
// making every task look live.
func isTerminalChatStatus(status string) bool {
	switch schemas.GraphStatus(status) {
	case schemas.GraphCompleted, schemas.GraphCompletedWithSkips,
		schemas.GraphFailed, schemas.GraphCancelled:
		return true
	}
	return false
}

// formatDuration renders a duration in a compact human-readable form suitable
// for a status line: "3m", "1h5m", "12s". Sub-second precision is dropped.
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
