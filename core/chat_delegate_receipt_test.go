package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestToolDefinitions_ExposeTaskStatus pins that a delegated task can be read
// back. The tool set used to be write_file/read_file/execute_code/delegate —
// with no way to observe a delegated task, the model paid a turn to start work
// it could never collect, while execute_code returns its output inline. That
// asymmetry is the mechanical reason delegation measured 0/104 tasks.
func TestToolDefinitions_ExposeTaskStatus(t *testing.T) {
	h := &ChatHarness{}
	names := map[string]bool{}
	for _, td := range h.ToolDefinitions() {
		names[td.Name] = true
	}
	for _, want := range []string{"delegate_to_orchestrator", "orchestrator_task_status"} {
		if !names[want] {
			t.Errorf("tool %q missing from the chat harness tool set: %v", want, names)
		}
	}
}

// TestDelegate_ReturnsReadableReceipt pins the receipt shape: an id alone is not
// actionable, so the result must name the follow-up call.
func TestDelegate_ReturnsReadableReceipt(t *testing.T) {
	h := &ChatHarness{
		Delegate: func(prompt string) (string, error) { return "task_abc123", nil },
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			return map[string]any{"status": "running"}, true
		},
	}

	out, err := h.execTool(context.Background(), "delegate_to_orchestrator",
		map[string]any{"prompt": "reconcile the three invoices"}, "chat_x")
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if !strings.Contains(out, "task_abc123") {
		t.Errorf("receipt must carry the task id, got:\n%s", out)
	}
	if !strings.Contains(out, "orchestrator_task_status") {
		t.Errorf("receipt must name the follow-up call, otherwise the id is a dead end, got:\n%s", out)
	}
	if !strings.Contains(out, "status=running") && !strings.Contains(out, "current status: running") {
		t.Errorf("receipt must report the task's current status, got:\n%s", out)
	}
}

// TestTaskStatus_FormatsTerminalTask pins that a finished task is reported with
// per-step outcomes and its artifacts — the things a caller needs in order to
// decide whether to trust the result.
func TestTaskStatus_FormatsTerminalTask(t *testing.T) {
	var gotWait time.Duration
	h := &ChatHarness{
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			gotWait = wait
			return map[string]any{
				"status": "completed_with_skips",
				"task":   "reconcile invoices",
				"steps": map[string]any{
					"s1": map[string]any{"status": "ok", "result_ref": "task_abc123:s1"},
					"s2": map[string]any{"status": "skipped"},
					"s3": map[string]any{"status": "failed", "last_error": "no such file"},
				},
				"output_files": []map[string]any{{"path": "review.txt"}},
			}, true
		},
	}

	out, err := h.execTool(context.Background(), "orchestrator_task_status",
		map[string]any{"task_id": "task_abc123", "wait_seconds": float64(20)}, "chat_x")
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	if gotWait != 20*time.Second {
		t.Errorf("wait_seconds=20 must reach the scheduler as 20s, got %v", gotWait)
	}
	for _, want := range []string{
		"status: completed_with_skips",
		"step s1: ok",
		"step s2: skipped",
		"step s3: failed (error: no such file)",
		"output_files: review.txt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status report missing %q, got:\n%s", want, out)
		}
	}
}

// TestTaskStatus_ClampsWait pins the bound: the chat turn is blocked while this
// waits, so a model asking for an hour must not get it.
func TestTaskStatus_ClampsWait(t *testing.T) {
	var gotWait time.Duration
	h := &ChatHarness{
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			gotWait = wait
			return map[string]any{"status": "running"}, true
		},
	}
	if _, err := h.execTool(context.Background(), "orchestrator_task_status",
		map[string]any{"task_id": "task_abc123", "wait_seconds": float64(3600)}, "chat_x"); err != nil {
		t.Fatalf("task_status: %v", err)
	}
	if gotWait != 60*time.Second {
		t.Errorf("wait must be clamped to 60s, got %v", gotWait)
	}
}

// TestSystemPrompt_StatesWhenToDelegate pins the other half of the same defect:
// the delegation section only ever listed prohibitions ("verify … first",
// "NEVER blindly delegate"). A rule that only says when not to act reads as
// "do not act", and the measured benchmark showed exactly that — 0 delegations,
// and the word "delegat*" in 0 of 73 final answers.
func TestSystemPrompt_StatesWhenToDelegate(t *testing.T) {
	h := &ChatHarness{}
	sys := h.buildSystem("reconcile three invoices", "task_t")
	if !strings.Contains(sys, "WHEN TO DELEGATE") {
		t.Fatalf("system prompt has no positive delegation trigger:\n%s", sys)
	}
	// The positive section must precede the gate, or the last instruction the
	// model reads is still a prohibition.
	if strings.Index(sys, "WHEN TO DELEGATE") > strings.Index(sys, "DELEGATION READINESS GATE") {
		t.Error("WHEN TO DELEGATE must come before the readiness gate")
	}
	// Both halves must survive: softening the gate is a separate, config-gated
	// decision (ReadinessGateSoft), not something this change may do.
	if !strings.Contains(sys, "DELEGATION READINESS GATE") {
		t.Error("the readiness gate disappeared from the prompt")
	}
}

// TestSystemPromptMarkers pins the literals that the T2 recorder uses to prove
// the delegation guidance reached the model.
//
// `_t2/fake_provider.py` records `SYSTEM_PROMPT_MARKERS` as hit/miss against the
// system prompt it actually received. The previous recorder kept the first 160
// bytes of the prompt, which could never contain "WHEN TO DELEGATE" — the
// section sits past that byte and the prompt is ~9 KB — so the witness was
// structurally incapable of being true and read as "the guidance is missing".
//
// Two copies of these strings is deliberate: a cross-repo import would mean this
// test cannot run without eval-kit checked out. The cost is that rewording a
// constant here breaks the eval side silently, so this test fails first and says
// where to update.
func TestSystemPromptMarkers(t *testing.T) {
	// Must stay in sync with SYSTEM_PROMPT_MARKERS in _t2/fake_provider.py.
	markers := []string{
		"WHEN TO DELEGATE",
		"DELEGATION READINESS GATE",
		"orchestrator_task_status",
	}
	sys := (&ChatHarness{}).buildSystem("reconcile three invoices", "task_t")
	for _, m := range markers {
		if !strings.Contains(sys, m) {
			t.Errorf("system prompt no longer contains %q. Update "+
				"_t2/fake_provider.py SYSTEM_PROMPT_MARKERS in the same commit, or the "+
				"T2 prompt witness silently reports a miss for a marker that was renamed.",
				m)
		}
	}
}
