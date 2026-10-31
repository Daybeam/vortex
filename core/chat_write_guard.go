package core

import (
	"fmt"
	"path"
	"strings"
)

// repeatedWriteThreshold is the number of writes to the same (tool, path)
// inside one turn before the model is steered away from writing it again.
//
// Why 3 and not 2: writing a file, reading it back and rewriting it once with a
// correction is normal work. The pathology this guard exists for is unbounded
// rewriting — 026 (harness-bench, 2026-10-30) issued 11 successive write_file
// calls to /workspace/review_findings.json inside one turn, each with a
// different `content`, and burned the entire 1200s budget without answering.
//
// Why this guard has to exist at all: every other repetition guard keys on the
// call signature, and a rewrite is never the same signature twice.
//   - cache replay (GetCachedResult) only covers read tools;
//   - ClearResultCache is called for write tools, so their results are never compared;
//   - CheckResultRepetition only looks at read-tool results;
//   - InspectToolCalls requires full argument equality, so a new `content` misses;
//   - the "no text output" nudge is non-latching and fired 4 times on 026,
//     each time ignored.
const repeatedWriteThreshold = 3

// writeToolArgKeys are the argument names a tool may use to name its output
// file, in priority order. The core tools (write_file / read_file) use "path";
// the rest are defensive, so a future tool that spells it differently is still
// counted instead of silently escaping the guard.
var writeToolArgKeys = []string{"path", "file_path", "target_path", "filename", "file"}

// writeTargetOf returns the normalized path a tool call writes to, or "" when
// the call does not target one named file.
//
// Returning "" is the common case and must stay cheap and quiet: read tools,
// pollers and code execution all touch the filesystem without naming a single
// output file, and counting them would fire the guard during normal work.
func (h *ChatHarness) writeTargetOf(toolName string, args map[string]any) string {
	if args == nil || isReadTool(toolName) || isPollerTool(toolName) {
		return ""
	}
	raw := ""
	for _, k := range writeToolArgKeys {
		if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return ""
	}
	root := ""
	if h != nil {
		root = h.SessionRoot
	}
	return normalizeWriteTarget(raw, root)
}

// writeGuardSteer records one successful write of target and returns the write
// index plus the steer text to append to the tool result ("" while the write
// index is still below repeatedWriteThreshold).
//
// Kept as a pure function on purpose: the semantics of the guard are then
// testable without touching the filesystem, so a host that locks a
// freshly-written file (Windows AV does exactly that) cannot disguise itself as
// a guard regression.
func writeGuardSteer(counts map[string]int, toolName, target string) (int, string) {
	if counts == nil || target == "" {
		return 0, ""
	}
	key := toolName + "\x00" + target
	counts[key]++
	n := counts[key]
	if n < repeatedWriteThreshold {
		return n, ""
	}
	return n, fmt.Sprintf(MarkerRepeatedWriteFmt, n, target)
}

// normalizeWriteTarget collapses the spellings of one file into a single key, so
// that "review_findings.json", "./review_findings.json" and
// "/workspace/review_findings.json" (with sessionRoot=/workspace) all count as
// the same target. Without the sessionRoot step an absolute-path write and a
// relative-path write to the same file would be counted separately, and 026 did
// exactly that kind of mixing.
func normalizeWriteTarget(p, sessionRoot string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, `\`, "/")
	p = path.Clean(p)
	if p == "." || p == "/" || p == "" {
		return ""
	}
	root := strings.TrimSpace(sessionRoot)
	if root != "" {
		root = path.Clean(strings.ReplaceAll(root, `\`, "/"))
		if root != "" && root != "/" && root != "." {
			if p == root {
				return ""
			}
			if strings.HasPrefix(p, root+"/") {
				p = strings.TrimPrefix(p, root+"/")
			}
		}
	}
	return p
}
