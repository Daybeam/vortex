package core

import (
	"strings"
	"testing"
)

// These pin the audit record for tool calls. The failure they exist for is
// harness-bench p23: six protected inputs vanished during a delegated run and
// the trace could not name the call that removed them, because every
// EventCoreToolCall said only {"tool": "execute_code", "status": "success"}.

func TestToolArgsForLog_KeepsSmallArgumentsWhole(t *testing.T) {
	rendered, total, paths := toolArgsForLog("read_file",
		map[string]any{"path": "/workspace/customers.csv"})

	if !strings.Contains(rendered, "customers.csv") {
		t.Errorf("small arguments must be recorded in full, got %q", rendered)
	}
	if strings.Contains(rendered, "truncated") {
		t.Errorf("small arguments must not be truncated: %q", rendered)
	}
	if total != len(rendered) {
		t.Errorf("arg_bytes must equal the rendered length when nothing was cut: %d vs %d",
			total, len(rendered))
	}
	if len(paths) != 1 || paths[0] != "/workspace/customers.csv" {
		t.Errorf("paths = %v, want [/workspace/customers.csv]", paths)
	}
}

// TestToolArgsForLog_PathSurvivesTruncation is the load-bearing case. A script
// whose destructive line sits past the byte budget must still leave the path on
// the record — otherwise the log answers "which tool" and not "which call",
// which is exactly as far as p23 got.
func TestToolArgsForLog_PathSurvivesTruncation(t *testing.T) {
	code := strings.Repeat("# padding line to push the interesting call out of the budget\n", 200) +
		"os.remove('/workspace/customers.csv')\n"
	rendered, total, paths := toolArgsForLog("execute_code",
		map[string]any{"code": code})

	if !strings.Contains(rendered, "truncated") {
		t.Fatalf("a %d-byte script must be truncated, got %d bytes", len(code), len(rendered))
	}
	if strings.Contains(rendered, "customers.csv") {
		t.Fatalf("test precondition broken: the path must NOT fit in the truncated text")
	}
	if total < 3000 {
		t.Errorf("arg_bytes must report the full size (%d), not the truncated one", total)
	}
	found := false
	for _, p := range paths {
		if strings.Contains(p, "customers.csv") {
			found = true
		}
	}
	if !found {
		t.Fatalf("paths must be extracted before truncation; got %v", paths)
	}
}

func TestToolArgsForLog_MasksCredentials(t *testing.T) {
	rendered, _, _ := toolArgsForLog("write_file", map[string]any{
		"path":    "/workspace/.env",
		"api_key": "sk-supersecretvalue123",
	})
	if strings.Contains(rendered, "supersecretvalue123") {
		t.Fatalf("credentials must not reach the log: %q", rendered)
	}
	if !strings.Contains(rendered, "sk-s****") {
		t.Errorf("expected a masked credential with a short prefix, got %q", rendered)
	}
}

// A tool that is not execute_code gets the smaller budget: 1200 bytes is enough
// for paths and small payloads, and keeps read/write-heavy runs from growing the
// jsonl by megabytes.
func TestToolArgsForLog_NonScriptToolGetsSmallerBudget(t *testing.T) {
	rendered, total, _ := toolArgsForLog("write_file", map[string]any{
		"path":    "/workspace/out.txt",
		"content": strings.Repeat("x", 5000),
	})
	if !strings.Contains(rendered, "truncated") {
		t.Fatal("a 5 KB write_file payload must be truncated")
	}
	if len(rendered) > toolArgsLogLimitBytes+64 {
		t.Errorf("rendered %d bytes, budget is %d", len(rendered), toolArgsLogLimitBytes)
	}
	if total < 5000 {
		t.Errorf("arg_bytes = %d, want the untruncated size", total)
	}
}

func TestToolCallLogDetail_Shape(t *testing.T) {
	detail := toolCallLogDetail("read_file", "error",
		map[string]any{"path": "/workspace/orders.csv"})

	for _, key := range []string{"tool", "status", "args", "arg_bytes"} {
		if _, ok := detail[key]; !ok {
			t.Errorf("detail is missing %q: %v", key, detail)
		}
	}
	if detail["status"] != "error" {
		t.Errorf("status = %v, want the resolved outcome, not the tool's claim", detail["status"])
	}
	if paths, ok := detail["paths"].([]string); !ok || len(paths) != 1 {
		t.Errorf("paths = %v, want one entry", detail["paths"])
	}

	// No path-like argument ⇒ no paths key at all, rather than an empty slice
	// that makes every event look like it had one.
	bare := toolCallLogDetail("execute_code", "success", map[string]any{"code": "print(1)"})
	if _, ok := bare["paths"]; ok {
		t.Errorf("a call with no file arguments must omit paths: %v", bare)
	}
}

func TestToolArgsForLog_NilArguments(t *testing.T) {
	rendered, total, paths := toolArgsForLog("read_file", nil)
	if rendered != "null" {
		t.Errorf("nil arguments render as null, got %q", rendered)
	}
	if total != len("null") {
		t.Errorf("arg_bytes = %d, want 4", total)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want none", paths)
	}
}

func TestToolArgsForLog_PathsAreDedupedAndStable(t *testing.T) {
	rendered, _, paths := toolArgsForLog("execute_code", map[string]any{
		"code": "open('b.csv'); open('a.csv'); open('b.csv')",
	})
	if !strings.Contains(rendered, "b.csv") {
		t.Fatalf("rendered lost the code: %q", rendered)
	}
	if len(paths) != 2 || paths[0] != "a.csv" || paths[1] != "b.csv" {
		t.Errorf("paths must be deduped and sorted for stable events, got %v", paths)
	}
}

// toolArgsForLog takes `any`, not map[string]any, because the inbound MCP path
// passes req.Params.Arguments — decoded JSON, which is only usually an object.
// These pin that the other shapes are still recorded rather than dropped, and
// that paths are still extracted from them.
func TestToolArgsForLog_NonObjectArguments(t *testing.T) {
	rendered, total, _ := toolArgsForLog("submit_task", "just a string")
	if !strings.Contains(rendered, "just a string") || total == 0 {
		t.Errorf("a bare string argument must still be recorded, got %q (total %d)", rendered, total)
	}

	// The interesting case: an array of file arguments. If this shape fell
	// through to a "not an object, skip it" branch, the inbound path would keep
	// the blind spot that made p23 unattributable.
	_, _, paths := toolArgsForLog("batch_ops", []any{"/w/a.csv", "/w/b.csv"})
	if len(paths) != 2 {
		t.Errorf("array arguments must still yield paths, got %v", paths)
	}
}

// The MCP path emitted nothing at all before this, so a sub-agent could delete
// a protected input through a filesystem server and leave no jsonl evidence
// that a call happened — the gap that made p23 unattributable. This pins the
// shape of the replacement record.
func TestMCPToolCallLogDetail_CarriesPathAndTransport(t *testing.T) {
	detail := mcpToolCallLogDetail("filesystem", "stdio", "delete_file",
		map[string]any{"path": "/workspace/customers.csv"})

	if detail["tool"] != "delete_file" {
		t.Errorf("tool = %v, want delete_file", detail["tool"])
	}
	if detail["mcp"] != "filesystem" || detail["transport"] != "stdio" {
		t.Errorf("the record must name which server and which transport: %v", detail)
	}
	if detail["status"] != "requested" {
		t.Errorf("status = %v, want the dispatch marker (the branch has six exits)", detail["status"])
	}
	paths, ok := detail["paths"].([]string)
	if !ok || len(paths) != 1 || paths[0] != "/workspace/customers.csv" {
		t.Fatalf("the destructive path must survive on the record, got %v", detail["paths"])
	}
}
