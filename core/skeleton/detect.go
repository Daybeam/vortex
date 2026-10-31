package skeleton

// detect.go — content-aware router over the skeletonizers in this package.
// Moved from core/chat_harness.go (artifact externalization) at the P2.5
// extraction (2026-10-22) so the package owns its whole routing surface and
// callers need only the three exported entries: FileSkeleton (extension
// based), SummarizeOutput (fallback), DetectAndSkeletonize (content based).

import (
	"encoding/json"
	"strings"
)

// DetectAndSkeletonize inspects raw content and applies the best
// skeletonizer. Detection is heuristic (prefix check), not perfect — it's an
// optimization, not a security boundary.
func DetectAndSkeletonize(data []byte, toolName string) string {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return "[empty]"
	}
	lower := strings.ToLower(trimmed)

	// HTML/SVG: starts with < (covers <html>, <!DOCTYPE>, <svg>, etc.)
	if strings.HasPrefix(lower, "<") {
		return tagSkeleton(data)
	}

	// JSON: starts with { or [
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if json.Valid(data) {
			// safeJSONSkeleton contains panic bugs here: this function runs on
			// arbitrary tool output inside a chat turn, where a panic costs the
			// whole turn (see safeJSONSkeleton's doc comment).
			return safeJSONSkeleton(data)
		}
	}

	// Code: if toolName is write_file/execute_code/read_file, apply code skeleton
	switch toolName {
	case "write_file", "execute_code", "read_file":
		if skel := codeSkeleton(data, goSymbols); len(skel) > 50 {
			return skel
		}
		if skel := codeSkeleton(data, jsSymbols); len(skel) > 50 {
			return skel
		}
		if skel := codeSkeleton(data, pySymbols); len(skel) > 50 {
			return skel
		}
	}

	// Fallback: head/tail + middle sampling
	return SummarizeOutput(data, toolName)
}
