package core

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"unicode/utf8"

	"github.com/daybeam/vortex/pkg/observability"
)

// Tool-call arguments: the half of the core tool log that was missing.
//
// EventCoreToolCall recorded {tool, status} and nothing else. What that costs is
// measurable: harness-bench p23 lost six protected input files during a delegated
// run, the parent agent's 107 recorded calls contained no delete at all (a regex
// sweep for os.remove / unlink / shutil / rm - / subprocess / os.system over
// every logged call returned zero), and the only window in which the files could
// have gone was the sub-agent's hand-off. The trace placed the loss to within a
// second and could not name the call, because no call wrote down its arguments.
//
// Three constraints keep the record affordable rather than merely bigger:
//
//   - size: arguments are truncated, and the untruncated byte count is logged
//     next to the text so a truncated record announces itself instead of looking
//     complete (a log that silently stops mid-script invites the wrong reading);
//   - secrets: values are masked with the same redactor the tracing exporter
//     uses, because a tool argument is exactly where an api_key appears;
//   - paths: file-like tokens are extracted from the *full* argument text before
//     truncation, so "which call touched customers.csv" stays answerable even
//     when the payload that mentions it was cut.
const (
	// toolArgsLogLimitBytes is the default budget for a recorded argument blob.
	// Small enough that a run's jsonl stays comparable in size to before, large
	// enough for read_file / write_file / path-style arguments in full.
	toolArgsLogLimitBytes = 1200

	// toolArgsScriptLogBytes is the budget for execute_code. Its argument is a
	// program, and the interesting line is as likely to be the last one as the
	// first; 3000 bytes covers the scripts observed in the benchmark traces.
	toolArgsScriptLogBytes = 3000

	// maxLoggedToolArgsPaths bounds the extracted path list. The point is to
	// keep the audit question answerable, not to reconstruct the argument.
	maxLoggedToolArgsPaths = 16
)

// toolArgPathPattern matches file-like tokens. Deliberately extension-anchored:
// a bare word would match every identifier in a script and drown the signal.
// The leading "/" is optional-and-included so an absolute path is recorded as
// one token ("/workspace/customers.csv"), not as the tail of itself
// ("workspace/customers.csv") — the audit question is usually "which path",
// and half a path answers a different one.
var toolArgPathPattern = regexp.MustCompile(
	`/?[A-Za-z0-9_][A-Za-z0-9_./\\-]*\.(?:csv|tsv|jsonl|json|txt|md|py|sh|go|ya?ml|toml|ini|xlsx?|parquet|zip|log)`)

// toolArgsForLog renders tool arguments for an event detail.
//
// args is `any` rather than map[string]any because the inbound MCP path
// (tools.loggedHandler) carries req.Params.Arguments, which is decoded JSON and
// only usually an object. A narrower signature would have forced that call site
// to keep logging the raw value — the one place where arguments could still
// reach the jsonl unbounded and unredacted.
//
// Returns the text to record, the size of the unredacted/untruncated arguments,
// and the paths found anywhere in them. Callers must log all three: the byte
// count is what makes truncation visible, and the paths are what survives it.
func toolArgsForLog(tool string, args any) (rendered string, totalBytes int, paths []string) {
	var raw string
	if m, ok := args.(map[string]any); ok {
		raw = marshalArgsForLog(m)
	} else if b, err := json.Marshal(args); err == nil {
		// nil, a bare string, an array: still worth recording, just not as an
		// object. json.Marshal(nil) yields "null", which reads correctly.
		raw = string(b)
	} else {
		raw = "<unmarshalable>"
	}
	if raw == "<unmarshalable>" {
		// Still worth recording that a call happened and its arguments were
		// unreadable; the size is unknown, so report 0 rather than the length of
		// the sentinel and let the reader see the marker itself.
		return raw, 0, nil
	}
	totalBytes = len(raw)
	paths = extractToolArgPaths(raw)

	safe := raw
	if redacted, err := json.Marshal(observability.RedactValue(args)); err == nil {
		safe = string(redacted)
	}

	limit := toolArgsLogLimitBytes
	if tool == "execute_code" {
		limit = toolArgsScriptLogBytes
	}
	rendered = safe
	if len(safe) > limit {
		// Cut on a rune boundary: a script with non-ASCII comments would
		// otherwise end mid-character and reach the log as U+FFFD.
		cut := limit
		for cut > 0 && !utf8.RuneStart(safe[cut]) {
			cut--
		}
		rendered = safe[:cut] + fmt.Sprintf("…[truncated %d of %d bytes]", len(safe)-cut, len(safe))
	}
	return rendered, totalBytes, paths
}

// toolArgsLogString is the text-only form, for events that want arguments but
// have no field plan of their own.
func toolArgsLogString(tool string, args any) string {
	s, _, _ := toolArgsForLog(tool, args)
	return s
}

// ToolArgsLogString is the exported form, for packages outside core that log
// tool arguments — currently tools.loggedHandler, which records every inbound
// MCP call from the parent agent.
//
// It exists so that path uses the same bounding and redaction as the rest
// rather than its own: a second implementation is how "arguments are recorded"
// quietly becomes "arguments are recorded on four paths out of five".
func ToolArgsLogString(tool string, args any) string {
	return toolArgsLogString(tool, args)
}

// toolCallLogDetail builds the EventCoreToolCall payload.
//
// status is the resolved outcome ("success" / "error"), not the tool's own
// claim: a call that returned an error string with a nil error must not read as
// a success here.
func toolCallLogDetail(tool, status string, args any) map[string]any {
	rendered, total, paths := toolArgsForLog(tool, args)
	detail := map[string]any{
		"tool":   tool,
		"status": status,
		"args":   rendered,
		// arg_bytes is the size before redaction and truncation. When it is much
		// larger than len(args), the record is partial — by design, but on the
		// record.
		"arg_bytes": total,
	}
	if len(paths) > 0 {
		detail["paths"] = paths
	}
	return detail
}

// mcpToolCallLogDetail builds the EventMCPToolCall payload.
//
// status is "requested", not a resolved outcome. The MCP branch settles its
// result in six different places (transport error, reconnect retry, SERF
// isError, the response payload's own status field, remote-vs-stdio), and a
// record that fires on only some of them reads as "the call never happened" on
// the rest. Emitting at dispatch is the one placement that covers every exit,
// and the arguments — which is what the audit actually asks for — are known
// there.
func mcpToolCallLogDetail(mcpID, transport, tool string, args any) map[string]any {
	detail := toolCallLogDetail(tool, "requested", args)
	detail["mcp"] = mcpID
	detail["transport"] = transport
	return detail
}

// extractToolArgPaths pulls file-like tokens out of raw argument JSON.
//
// It runs on the full text on purpose: truncation is what the paths have to
// survive. Order is stable (sorted) so two runs of the same call produce the
// same event.
func extractToolArgPaths(raw string) []string {
	matches := toolArgPathPattern.FindAllString(raw, maxLoggedToolArgsPaths*4)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, maxLoggedToolArgsPaths)
	for _, m := range matches {
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
		if len(out) >= maxLoggedToolArgsPaths {
			break
		}
	}
	sort.Strings(out)
	return out
}
