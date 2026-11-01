package core

import (
	"encoding/json"
	"testing"

	"github.com/daybeam/vortex/store"
)

// D4: `orchestrator_task_status` carries none of the poller name signals and
// starts with no read verb, so the engine treated it as a write. Three things
// followed, all observed on 026/v65: its result was prefixed
// "[ACTION COMPLETED]", every poll invalidated the read cache, and five
// legitimate polls with the same (task_id, wait_seconds=20) were counted as an
// infinite loop.
//
// The fix judges the CALL, not just the name.

func TestIsPollingCall_BlockingWaitMakesAnyToolAPoll(t *testing.T) {
	// A caller that asks to block until something changes is polling, whatever
	// the tool is called. This is the signal that covers tools we do not own.
	for name, args := range map[string]map[string]any{
		"float64": {"wait_seconds": float64(20)},
		"int":     {"wait_seconds": 20},
		"int64":   {"wait_seconds": int64(5)},
		"number":  {"wait_seconds": json.Number("30")},
	} {
		if !isPollingCall("vendor__job_state", args) {
			t.Errorf("%s: a call with wait_seconds>0 must be treated as polling", name)
		}
	}

	// Zero or negative is not a request to block.
	for _, args := range []map[string]any{
		{"wait_seconds": float64(0)},
		{"wait_seconds": float64(-1)},
		{},
		nil,
	} {
		if isPollingCall("vendor__job_state", args) {
			t.Errorf("args %v must not be treated as polling", args)
		}
	}
}

func TestIsPollingCall_TaskStatusIsVolatileWithoutWait(t *testing.T) {
	// With wait_seconds=0 the call returns "running" now and "completed" a
	// second later. Caching that would hand the model a frozen answer, so the
	// built-in delegation status query is a poll even without a blocking arg.
	for _, name := range []string{
		"orchestrator_task_status",
		"orchestrator__task_status",
	} {
		if !isPollingCall(name, map[string]any{"task_id": "task_1"}) {
			t.Errorf("%s must be treated as polling even with wait_seconds=0", name)
		}
	}
}

func TestIsPollingCall_WithoutBlockingSignalIsNotAPoll(t *testing.T) {
	// The guard must not be widened into "anything with status in the name":
	// `update_status` is a write, and treating it as a poll would skip the
	// cache invalidation it owes.
	for _, name := range []string{
		"read_file", "write_file", "execute_code", "update_status",
		"submit_task", "get_status",
	} {
		if isPollingCall(name, map[string]any{"path": "/workspace/a.txt"}) {
			t.Errorf("%s must not be treated as polling", name)
		}
	}
}

func TestToolResultPolicyFor(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want toolResultPolicy
	}{
		{"orchestrator_task_status", map[string]any{"task_id": "t", "wait_seconds": 20}, alwaysFreshResult},
		{"orchestrator_task_status", map[string]any{"task_id": "t"}, alwaysFreshResult},
		{"vendor__job_state", map[string]any{"wait_seconds": 5}, alwaysFreshResult},
		{"read_file", map[string]any{"path": "/workspace/a.txt"}, cacheReadResult},
		{"search_archive", map[string]any{"q": "x"}, cacheReadResult},
		{"write_file", map[string]any{"path": "/workspace/a.txt"}, actionResult},
		{"execute_code", map[string]any{"code": "print(1)"}, actionResult},
		{"update_status", map[string]any{"status": "done"}, actionResult},
	}
	for _, c := range cases {
		if got := toolResultPolicyFor(c.name, c.args); got != c.want {
			t.Errorf("toolResultPolicyFor(%q, %v) = %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

// The loop guard must not fire on the documented way to wait for a delegated
// task: same task_id, same wait_seconds, once per model turn.
func TestInspectToolCalls_ExemptsLegitimateStatusPolling(t *testing.T) {
	for _, args := range []map[string]any{
		{"task_id": "task_abc", "wait_seconds": 20},
		{"task_id": "task_abc"}, // no blocking arg, still volatile
	} {
		s := NewSieve(10)
		s.ToolRepetitionThreshold = 3
		call := store.ToolInteraction{ToolName: "orchestrator_task_status", Arguments: args}
		for i := 0; i < 8; i++ {
			if ok, reason := s.InspectToolCalls("task-poll", "", []store.ToolInteraction{call}); !ok {
				t.Fatalf("poll #%d with args %v was blocked: %s", i+1, args, reason)
			}
		}
	}
}

// ...and it must still fire when the same call is genuinely repetitive.
func TestInspectToolCalls_StillBlocksIdenticalReads(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3
	call := store.ToolInteraction{ToolName: "read_file", Arguments: map[string]any{"path": "/workspace/a.txt"}}

	blocked := false
	for i := 0; i < 8 && !blocked; i++ {
		ok, reason := s.InspectToolCalls("task-read", "", []store.ToolInteraction{call})
		if !ok {
			blocked = true
			if reason != "InfiniteLoopDetected" {
				t.Fatalf("unexpected block reason: %s", reason)
			}
		}
	}
	if !blocked {
		t.Fatal("eight identical read_file calls were never blocked; the guard was weakened")
	}
}

func TestInspectToolCalls_StillBlocksIdenticalWrites(t *testing.T) {
	s := NewSieve(10)
	s.ToolRepetitionThreshold = 3
	call := store.ToolInteraction{ToolName: "write_file", Arguments: map[string]any{"path": "/workspace/a.txt"}}

	blocked := false
	for i := 0; i < 8 && !blocked; i++ {
		ok, _ := s.InspectToolCalls("task-write", "", []store.ToolInteraction{call})
		if !ok {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("eight identical write_file calls were never blocked; the guard was weakened")
	}
}
