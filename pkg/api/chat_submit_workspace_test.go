package api

import (
	"os"
	"strings"
	"testing"

	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
)

// harness-bench D3 (2026-10-30): the chat gateway knows the workspace root
// (ChatHarness.SessionRoot, from HARNESSBENCH_WORKSPACE) but all four of its
// submission paths passed workspaceRoot="". A delegated task therefore ran in
// outputs/<taskID> — a directory holding only manifest.json — while the chat
// agent that delegated it had been happily reading /workspace. "Delegate a
// subagent" silently meant "hand the work to an agent that cannot see it".
//
// TestSubmitChatTask_CarriesHarnessWorkspaceRoot is the behavioural half: the
// root survives submitChatTask into the task graph, which is where
// scheduler_dag_exec reads it to fill SpawnRequest.SessionRoot.
//
// TestChatHandlers_SubmitThroughOnePath is the structural half: the regression
// mode is a *fifth* submission site that compiles fine and drops the root, so
// the guard counts call sites rather than trusting review.
func TestSubmitChatTask_CarriesHarnessWorkspaceRoot(t *testing.T) {
	h := newTestChatHandler(t)
	ws := t.TempDir()
	h.Harness = &chat.ChatHarness{SessionRoot: ws}

	id, err := h.submitChatTask([]schemas.StepInput{{
		ID:   "s1",
		Task: "Read report.md and write review_findings.json",
	}}, "sess-d3")
	if err != nil {
		t.Fatalf("submitChatTask: %v", err)
	}

	st, ok := h.App.Scheduler.GetStatus(id, "full")
	if !ok {
		t.Fatalf("no status for task %s", id)
	}
	got, _ := st["workspace_root"].(string)
	if got != ws {
		t.Fatalf("workspace_root = %q, want %q\n"+
			"The delegated agent will resolve paths against outputs/<taskID> "+
			"and will not see the files the chat agent was reading.", got, ws)
	}
}

func TestSubmitChatTask_KeepsLegacyRootWhenUnconfigured(t *testing.T) {
	h := newTestChatHandler(t)
	h.Harness = &chat.ChatHarness{} // no HARNESSBENCH_WORKSPACE

	id, err := h.submitChatTask([]schemas.StepInput{{
		ID:   "s1",
		Task: "Read report.md and write review_findings.json",
	}}, "sess-legacy")
	if err != nil {
		t.Fatalf("submitChatTask: %v", err)
	}

	st, ok := h.App.Scheduler.GetStatus(id, "full")
	if !ok {
		t.Fatalf("no status for task %s", id)
	}
	if got, _ := st["workspace_root"].(string); got != "" {
		t.Fatalf("workspace_root = %q, want empty — deployments that configure "+
			"no workspace root must keep per-task output directories", got)
	}
}

// The structural guard. The regression mode is a fifth submission site that
// compiles fine and passes an empty root, so the check is deliberately about
// provenance rather than count: every Scheduler.Submit call in the file must be
// visibly fed a workspace root — either chatWorkspaceRoot(...) or a local
// variable derived from it (wsRoot).
//
// Call sites may legitimately differ (chatWorkspaceRoot for the harness tree,
// wsRoot for the per-session upload sandbox), so this does not demand one
// spelling, only that the root is present and traceable.
func TestChatHandlers_NoSubmitWithoutWorkspaceRoot(t *testing.T) {
	src, err := os.ReadFile("chat_handlers.go")
	if err != nil {
		t.Fatalf("read chat_handlers.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")

	// A submit call may span many lines. Only look forward: the root has to
	// appear in the call's own arguments, not merely be declared above it —
	// otherwise `wsRoot := ...` followed by `..., ""` would still pass.
	const before, after = 0, 14
	found := 0
	for i, line := range lines {
		idx := strings.Index(line, "Scheduler.Submit")
		if idx < 0 {
			continue
		}
		found++
		lo, hi := i-before, i+after
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines) {
			hi = len(lines)
		}
		block := strings.Join(lines[lo:hi], "\n")
		if !strings.Contains(block, "chatWorkspaceRoot") && !strings.Contains(block, "wsRoot") {
			t.Errorf("chat_handlers.go:%d submits a task without a workspace root:\n\t%s\n"+
				"Route it through h.submitChatTask (which derives the root from the "+
				"harness) or pass a root explicitly. An empty root sends the delegated "+
				"agent to outputs/<taskID>, which is D3 all over again.",
				i+1, strings.TrimSpace(line))
		}
	}
	if found == 0 {
		t.Fatal("no Scheduler.Submit call found — did the submit path get renamed? " +
			"This guard is only meaningful if it can see the calls.")
	}
}
