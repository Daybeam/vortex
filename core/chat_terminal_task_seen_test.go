package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// OnTerminalTaskSeen is the signal the API layer uses to decide whether a
// follow-up wake is a duplicate or a first delivery. These tests pin both
// directions, because either mistake is silent:
//
//   - missing a terminal state ⇒ the queued wake is delivered anyway ⇒ a second
//     answer and a second turn for work the model already read;
//   - reporting a non-terminal state ⇒ the queued wake is suppressed ⇒ the
//     result is never delivered, which is the defect being fixed.
func TestExecTool_TaskStatusReportsTerminalState(t *testing.T) {
	var seen []string
	h := &ChatHarness{
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			return map[string]any{"status": string(schemas.GraphCompleted)}, true
		},
		OnTerminalTaskSeen: func(id string) { seen = append(seen, id) },
	}

	out, err := h.execTool(context.Background(), "orchestrator_task_status",
		map[string]any{"task_id": "task-term"}, "chat_x")
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	if out == "" {
		t.Fatal("task_status returned an empty rendering")
	}
	if len(seen) != 1 || seen[0] != "task-term" {
		t.Fatalf("expected exactly one terminal notification for task-term, got %v", seen)
	}
}

func TestExecTool_TaskStatusDoesNotReportRunningState(t *testing.T) {
	var seen []string
	h := &ChatHarness{
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			return map[string]any{"status": string(schemas.GraphRunning)}, true
		},
		OnTerminalTaskSeen: func(id string) { seen = append(seen, id) },
	}

	if _, err := h.execTool(context.Background(), "orchestrator_task_status",
		map[string]any{"task_id": "task-live"}, "chat_x"); err != nil {
		t.Fatalf("task_status: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("a running task is not a delivered result; got notifications %v", seen)
	}
}

// A missing task must not be reported as consumed: the model learned nothing,
// so a queued wake for that id (if one exists) still has to run.
func TestExecTool_TaskStatusMissingTaskDoesNotReport(t *testing.T) {
	var seen []string
	h := &ChatHarness{
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			return nil, false
		},
		OnTerminalTaskSeen: func(id string) { seen = append(seen, id) },
	}

	if _, err := h.execTool(context.Background(), "orchestrator_task_status",
		map[string]any{"task_id": "task-gone"}, "chat_x"); err == nil {
		t.Fatal("expected an error for a task the orchestrator does not know")
	}
	if len(seen) != 0 {
		t.Fatalf("an unknown task is not a delivered result; got notifications %v", seen)
	}
}

// Every terminal status counts, not just "completed": a failed task's result is
// still the result the session was waiting for, and suppressing its wake would
// leave the model with a task it never hears about again.
func TestExecTool_TaskStatusReportsEveryTerminalStatus(t *testing.T) {
	for _, status := range []string{
		string(schemas.GraphCompleted),
		string(schemas.GraphCompletedWithSkips),
		string(schemas.GraphFailed),
		string(schemas.GraphCancelled),
	} {
		var seen []string
		h := &ChatHarness{
			DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
				return map[string]any{"status": status}, true
			},
			OnTerminalTaskSeen: func(id string) { seen = append(seen, id) },
		}
		if _, err := h.execTool(context.Background(), "orchestrator_task_status",
			map[string]any{"task_id": "task-s"}, "chat_x"); err != nil {
			t.Fatalf("%s: task_status: %v", status, err)
		}
		if len(seen) != 1 {
			t.Errorf("status %q must be reported as terminal, got %v", status, seen)
		}
	}
}

// The receipt goes through the same path: a task that is already terminal when
// the receipt is built has been shown to the model just as surely as one read
// through the status tool.
func TestDelegateReceipt_ReportsAlreadyTerminalTask(t *testing.T) {
	var seen []string
	h := &ChatHarness{
		Delegate: func(prompt string) (string, error) { return "task-fast", nil },
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			return map[string]any{"status": string(schemas.GraphCompleted)}, true
		},
		OnTerminalTaskSeen: func(id string) { seen = append(seen, id) },
	}

	if _, err := h.execTool(context.Background(), "delegate_to_orchestrator",
		map[string]any{"prompt": "do the thing"}, "chat_x"); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if len(seen) != 1 || seen[0] != "task-fast" {
		t.Fatalf("expected one terminal notification from the receipt, got %v", seen)
	}
}

// A nil callback is the default (no consumer wired); it must not panic.
func TestExecTool_TaskStatusWithoutConsumerIsSafe(t *testing.T) {
	h := &ChatHarness{
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			return map[string]any{"status": string(schemas.GraphCompleted)}, true
		},
	}
	if _, err := h.execTool(context.Background(), "orchestrator_task_status",
		map[string]any{"task_id": "task-none"}, "chat_x"); err != nil {
		t.Fatalf("task_status: %v", err)
	}
}
