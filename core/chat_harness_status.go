package core

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

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
func formatTaskStatus(id string, st map[string]any) string {
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
		case root != "":
			b.WriteString("this task is finished and declared no output files: the agents write " +
				"into the workspace, so list " + root + " (e.g. execute_code with `ls -la`) " +
				"to find the deliverable. Do not poll this tool again for this task.\n")
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
