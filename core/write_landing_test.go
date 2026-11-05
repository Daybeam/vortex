package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// harness-bench 011 (2026-11-03): `write_file({"path":"workspace/x.md"})` under
// SessionRoot=/workspace landed in /workspace/workspace/x.md, which is outside
// the task's contract, while the tool result said nothing unusual. These tests
// pin the correction and, just as important, pin its narrowness: a genuine
// subdirectory must keep landing where the caller asked.

func nestedWorkspaceValidator(t *testing.T) (*SafePathValidator, string) {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	taskDir := filepath.Join(base, "task")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatalf("mkdir task: %v", err)
	}
	return NewSafePathValidator(taskDir, ws, nil), ws
}

func TestCorrectNestedSessionPath(t *testing.T) {
	v, ws := nestedWorkspaceValidator(t)
	sep := string(filepath.Separator)
	cases := []struct {
		rel      string
		wantPath string
		wantHit  bool
	}{
		// The reported defect: the leading segment only repeats the root name.
		{"workspace/customer_reply.md", "customer_reply.md", true},
		{filepath.Join("workspace", "out", "x.json"), filepath.Join("out", "x.json"), true},
		// Genuine subdirectories are untouched.
		{"out/customer_reply.md", "", false},
		{"reports/2026/x.csv", "", false},
		// Absolute paths are somebody else's decision (resolvePath validates them).
		{sep + filepath.Join("workspace", "x.md"), "", false},
		// Nothing to correct.
		{"workspace", "", false},
		{"workspace" + sep, "", false},
		{"x.md", "", false},
		// Cleaning happens first: "workspace/../x.md" is really "x.md".
		{"workspace/../x.md", "", false},
		// Traversal out of the corrected path is refused.
		{"workspace/../../etc/passwd", "", false},
	}
	for _, c := range cases {
		got, hit := correctNestedSessionPath(c.rel, v)
		if hit != c.wantHit || (hit && got != c.wantPath) {
			t.Errorf("correctNestedSessionPath(%q) = (%q,%v), want (%q,%v) [root=%s]",
				c.rel, got, hit, c.wantPath, c.wantHit, ws)
		}
	}
}

func TestCorrectNestedSessionPath_NilAndRootlessValidator(t *testing.T) {
	if _, hit := correctNestedSessionPath("workspace/x.md", nil); hit {
		t.Error("a nil validator must not rewrite anything")
	}
	if _, hit := correctNestedSessionPath("workspace/x.md", &SafePathValidator{TaskDir: t.TempDir()}); hit {
		t.Error("without a SessionRoot there is no directory name to duplicate")
	}
}

// The correction must be visible in the result: the whole defect was that the
// transcript looked correct while the file went somewhere else.
func TestHandleCoreTool_WriteFileCorrectsNestedPathAndSaysSo(t *testing.T) {
	v, ws := nestedWorkspaceValidator(t)
	out, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "workspace/customer_reply.md",
		"content": "hi Dana",
	}, ws, "task1", v)
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "customer_reply.md")); err != nil {
		t.Errorf("deliverable must land at the workspace root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "workspace", "customer_reply.md")); err == nil {
		t.Errorf("the nested copy must not be written at all")
	}
	if !strings.Contains(out, "[path]") || !strings.Contains(out, "customer_reply.md") {
		t.Errorf("the result must name the corrected landing point, got:\n%s", out)
	}
}

// Reverse case: a real subdirectory keeps its path and gets no note.
func TestHandleCoreTool_WriteFileKeepsGenuineSubdirectory(t *testing.T) {
	v, ws := nestedWorkspaceValidator(t)
	out, err := HandleCoreTool(context.Background(), "write_file", map[string]any{
		"path":    "out/customer_reply.md",
		"content": "hi Dana",
	}, ws, "task1", v)
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "out", "customer_reply.md")); err != nil {
		t.Errorf("a genuine subdirectory must be honoured: %v", err)
	}
	if strings.Contains(out, "[path]") {
		t.Errorf("no correction happened, so there must be no [path] note, got:\n%s", out)
	}
}
