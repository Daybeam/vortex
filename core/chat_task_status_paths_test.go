package core

import (
	"path/filepath"
	"strings"
	"testing"
)

// harness-bench D5 (2026-10-30).
//
// A delegated task that finished came back as "status: completed / step s1: ok"
// and nothing else. The step agent had written its deliverable with write_file,
// which never reaches graph.OutputFiles (that list only gains entries from a
// declared output_ref or a side-loaded JSON asset), so the model had no way to
// learn where the files went. Its only available action was to ask again: trial
// 026 called orchestrator_task_status 34 times across 40 turns, ran out of
// turns, and scored 0 with review_findings.json sitting in the workspace.
//
// The fix is not "more status" — it is naming the location and saying that
// further polling cannot help.

func TestFormatTaskStatus_NamesWorkspaceForUndeclaredOutput(t *testing.T) {
	ws := filepath.Join(string(filepath.Separator), "workspace")
	out := formatTaskStatus("task_1", map[string]any{
		"status":         "completed",
		"workspace_root": ws,
		"steps":          map[string]any{"s1": map[string]any{"status": "ok"}},
	})

	if !strings.Contains(out, "workspace_root: "+ws) {
		t.Fatalf("status must say where the task worked, got:\n%s", out)
	}
	if !strings.Contains(out, "ls") {
		t.Errorf("with no declared outputs the report must say how to find them "+
			"(list the workspace), got:\n%s", out)
	}
	if !strings.Contains(out, "Do not poll this tool again") {
		t.Errorf("a finished task must say that polling again is pointless, got:\n%s", out)
	}
}

func TestFormatTaskStatus_AbsolutisesDeclaredOutputs(t *testing.T) {
	ws := filepath.Join(string(filepath.Separator), "workspace")
	out := formatTaskStatus("task_1", map[string]any{
		"status":         "completed",
		"workspace_root": ws,
		"output_files":   []map[string]any{{"path": "reports/findings.md"}},
	})

	want := filepath.Join(ws, "reports", "findings.md")
	if !strings.Contains(out, want) {
		t.Fatalf("declared outputs must be reported as absolute paths so the model "+
			"can open them, want %q in:\n%s", want, out)
	}
	if !strings.Contains(out, "read the output_files") {
		t.Errorf("a finished task with outputs must point at them, got:\n%s", out)
	}
}

// A running task must not be handed a "finished" instruction: the model would
// stop waiting and answer from nothing.
func TestFormatTaskStatus_RunningGetsNoTerminalAdvice(t *testing.T) {
	out := formatTaskStatus("task_1", map[string]any{
		"status":         "running",
		"workspace_root": "/workspace",
	})
	if strings.Contains(out, "this task is finished") {
		t.Fatalf("a running task must not be reported as finished:\n%s", out)
	}
}

// Backward compatibility: no workspace root configured means the old rendering,
// which deployments without one still depend on.
func TestFormatTaskStatus_LegacyWithoutWorkspaceRoot(t *testing.T) {
	out := formatTaskStatus("task_1", map[string]any{
		"status":       "completed",
		"output_files": []map[string]any{{"path": "review.txt"}},
	})
	if strings.Contains(out, "workspace_root:") {
		t.Errorf("no workspace root was set; the report must not invent one:\n%s", out)
	}
	if !strings.Contains(out, "output_files: review.txt") {
		t.Errorf("legacy rendering changed, got:\n%s", out)
	}
}

// The terminal set is the one the engine actually emits. Getting this wrong in
// the permissive direction makes every task look finished.
func TestIsTerminalChatStatus(t *testing.T) {
	terminal := []string{"completed", "completed_with_skips", "failed", "cancelled"}
	for _, s := range terminal {
		if !isTerminalChatStatus(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	live := []string{"running", "blocked", "pending_review", "suspended", ""}
	for _, s := range live {
		if isTerminalChatStatus(s) {
			t.Errorf("%q must not be treated as terminal", s)
		}
	}
}
