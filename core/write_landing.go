package core

import (
	"path/filepath"
	"strings"
)

// Nested-workspace landing correction.
//
// `write_file({"path": "workspace/customer_reply.md"})` with
// SessionRoot=/workspace used to resolve to /workspace/workspace/customer_reply.md:
// resolvePath joins SessionRoot, MkdirAll creates the missing directory, and the
// tool result echoes the same relative path back, so nothing in the transcript
// looks wrong. Measured on harness-bench 011 (2026-11-03): 4 of 80 graded runs
// did exactly this, and two of them scored 0.00 — the parent could not find the
// deliverable, rebuilt it itself with a misspelled key, and burned the request
// budget. The grader reads /workspace/customer_reply.md, so the write landed
// outside the contract while every layer reported success.
//
// The rule is deliberately narrow: the first segment must be *exactly* the base
// name of SessionRoot. "out/report.md", "src/main.go" and any other genuine
// subdirectory are untouched, so a task that writes into subdirectories keeps
// working. A blanket "strip the first segment" rule would rewrite those too;
// rejecting relative paths outright would break the common case, because
// subagents overwhelmingly write relative paths and they are correct to.
//
// Why not gate on the task's declared artifact list: that list is
// mutation_contract.allowed_created (run_benchmark.py renders it as the
// "- Create:" line), and it differs from task.yaml's required_artifacts on
// 185 of 274 tasks. Gating on it would stay silent on exactly the files a task
// needs but was not allowed to create — the case where the silent misplacement
// hurts most. The declared list still has a use (see the receipt in
// pkg/chat/chat_harness_status.go); it is just not the signal here.
func correctNestedSessionPath(rel string, v *SafePathValidator) (string, bool) {
	if v == nil || v.SessionRoot == "" {
		return "", false
	}
	cleaned := filepath.Clean(rel)
	if cleaned == "" || cleaned == "." || filepath.IsAbs(cleaned) {
		return "", false
	}
	rootName := filepath.Base(filepath.Clean(v.SessionRoot))
	if rootName == "" || rootName == "." || rootName == string(filepath.Separator) {
		return "", false
	}
	parts := strings.Split(cleaned, string(filepath.Separator))
	if len(parts) < 2 || parts[0] != rootName {
		return "", false
	}
	rest := filepath.Join(parts[1:]...)
	// Never turn a write into a traversal: the remainder must still be a plain
	// relative path inside the workspace root.
	if rest == "" || rest == "." || rest == ".." ||
		strings.HasPrefix(rest, ".."+string(filepath.Separator)) || filepath.IsAbs(rest) {
		return "", false
	}
	return rest, true
}

// writeLandingNote is the one line appended to a write_file result when the
// landing point was corrected. It names both paths so a trace reader — and the
// model itself — can see that the nested copy was not written.
func writeLandingNote(original, corrected, sessionRoot string) string {
	return "[path] " + quoteForNote(original) + " resolved to " + quoteForNote(corrected) +
		": the leading " + quoteForNote(filepath.Base(filepath.Clean(sessionRoot))) +
		" only repeats the workspace directory (" + sessionRoot +
		"), so the nested copy would sit outside the workspace root."
}

func quoteForNote(s string) string { return "'" + s + "'" }
