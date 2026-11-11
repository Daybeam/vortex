package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestPostDelegPollModeFromEnv pins the same rule as the lockout arm: only a
// recognised value turns the lever on, a typo leaves behaviour alone.
func TestPostDelegPollModeFromEnv(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"off", false},
		{"0", false},
		{"wake", true},
		{"WAKE", true},
		{"1", true},
		{"true", true},
		{"on", true},
		{"wkae", false},
	}
	for _, c := range cases {
		t.Setenv(PostDelegPollEnv, c.raw)
		p := &postDelegState{}
		if got := p.pollWakeMode(); got != c.want {
			t.Errorf("%s=%q -> %v, want %v", PostDelegPollEnv, c.raw, got, c.want)
		}
	}
}

// statusProbe is a DelegateStatus stand-in that records the waits it was asked
// for and returns a fixed status.
func statusProbe(status string, waits *[]time.Duration) func(string, time.Duration) (map[string]any, bool) {
	return func(id string, wait time.Duration) (map[string]any, bool) {
		*waits = append(*waits, wait)
		return map[string]any{"status": status, "steps": map[string]any{}}, true
	}
}

// Regression test for the polling lever (§9.12.5): without it every status call
// blocks for wait_seconds and the model keeps polling (~3.3 polls per run,
// §9.12.2). With VORTEX_POST_DELEG_POLL=wake the first check must return
// immediately with a stop-polling instruction, and every later one must be
// refused rather than executed.
func TestWakeArmFirstPollIsImmediateAndLaterPollsAreLocked(t *testing.T) {
	t.Setenv(PostDelegPollEnv, "wake")
	var waits []time.Duration
	h := &ChatHarness{DelegateStatus: statusProbe("running", &waits)}
	h.pd.recordDelegation("task_w")

	args := map[string]any{"task_id": "task_w", "wait_seconds": float64(20)}
	out, err := h.execTool(context.Background(), "orchestrator_task_status", args, "run1")
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if len(waits) != 1 || waits[0] != 0 {
		t.Fatalf("the first check must not block, waits=%v", waits)
	}
	if !strings.Contains(out, "still running") || !strings.Contains(out, "Do not call orchestrator_task_status again") {
		t.Errorf("the immediate answer must say the wake is coming and polling is over:\n%s", out)
	}
	if h.pd.polls != 1 {
		t.Fatalf("polls=%d, want 1", h.pd.polls)
	}

	if !h.pd.blocksPoll("orchestrator_task_status", args) {
		t.Fatal("the second poll for the delegated task must be refused")
	}
	if h.pd.pollBlocked != 1 {
		t.Errorf("pollBlocked=%d, want 1", h.pd.pollBlocked)
	}
	if len(waits) != 1 {
		t.Errorf("a refused poll must never reach DelegateStatus, waits=%v", waits)
	}

	// End to end through beforeToolCall: the model sees the lockout text, the
	// call is skipped, and it does not inflate calls_after_deleg.
	var events []string
	messages := []ChatMessage{}
	skipped := h.beforeToolCall(
		schemas.ToolCall{Name: "orchestrator_task_status", Arguments: args},
		"run1",
		func(ev ChatEvent) error { events = append(events, ev.Type); return nil },
		&messages)
	if !skipped {
		t.Fatal("beforeToolCall must skip the second poll")
	}
	if h.pd.after != 0 {
		t.Errorf("a refused poll ran nothing; calls_after_deleg=%d, want 0", h.pd.after)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Content, "LOCKED") {
		t.Fatalf("the model must be told why the call was refused, got %+v", messages)
	}
	found := false
	for _, e := range events {
		if e == "post_deleg_poll_blocked" {
			found = true
		}
	}
	if !found {
		t.Errorf("the refusal must be observable in events, got %v", events)
	}
}

// The lever is about the task this run handed off; a status call for any other
// task must pass through untouched.
func TestWakeArmDoesNotBlockOtherTasks(t *testing.T) {
	t.Setenv(PostDelegPollEnv, "wake")
	h := &ChatHarness{}
	h.pd.recordDelegation("task_w")
	h.pd.polls = 1

	if h.pd.blocksPoll("orchestrator_task_status", map[string]any{"task_id": "task_other"}) {
		t.Fatal("a poll for a different task must not be refused")
	}
	if h.pd.pollBlocked != 0 {
		t.Fatalf("pollBlocked=%d, want 0", h.pd.pollBlocked)
	}
	if h.pd.isWakePoll("task_other") {
		t.Fatal("the wake shortcut must only apply to the delegated task")
	}
}

// Off by default: the first poll blocks exactly as before, no wake note is
// appended, and nothing is refused. The poll counter still runs so the off arm
// yields the baseline from the same binary.
func TestPollLeverOffKeepsBlockingPoll(t *testing.T) {
	t.Setenv(PostDelegPollEnv, "")
	var waits []time.Duration
	h := &ChatHarness{DelegateStatus: statusProbe("running", &waits)}
	h.pd.recordDelegation("task_w")

	args := map[string]any{"task_id": "task_w", "wait_seconds": float64(20)}
	out, err := h.execTool(context.Background(), "orchestrator_task_status", args, "run1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(waits) != 1 || waits[0] != 20*time.Second {
		t.Fatalf("off arm must honour wait_seconds, waits=%v", waits)
	}
	if strings.Contains(out, "still running") {
		t.Errorf("off arm must not add the wake note:\n%s", out)
	}
	if h.pd.blocksPoll("orchestrator_task_status", args) {
		t.Fatal("off arm must never refuse a poll")
	}
	if h.pd.polls != 1 {
		t.Errorf("off arm must still count polls, got %d", h.pd.polls)
	}
}

// A first poll that lands on a finished task needs no "wait for the wake" line:
// formatTaskStatus already gives the terminal next action.
func TestWakeNoteOnlyWhileRunning(t *testing.T) {
	t.Setenv(PostDelegPollEnv, "wake")
	var waits []time.Duration
	h := &ChatHarness{DelegateStatus: statusProbe("completed", &waits)}
	h.pd.recordDelegation("task_w")

	out, err := h.execTool(context.Background(), "orchestrator_task_status",
		map[string]any{"task_id": "task_w"}, "run1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if strings.Contains(out, "still running") {
		t.Errorf("a terminal status must not tell the model to wait:\n%s", out)
	}
	if h.pd.polls != 1 {
		t.Errorf("polls=%d, want 1", h.pd.polls)
	}
}

// The receipt is the first thing the model reads after delegating. In the wake
// arm it must not advertise a poll that the very next call would refuse.
func TestDelegateReceiptWakeArmSaysDoNotPoll(t *testing.T) {
	t.Setenv(PostDelegPollEnv, "wake")
	var waits []time.Duration
	h := &ChatHarness{DelegateStatus: statusProbe("running", &waits)}
	h.pd.recordDelegation("task_w")

	receipt := h.delegateReceipt("task_w")
	if !strings.Contains(receipt, "Do not poll orchestrator_task_status") {
		t.Errorf("wake-arm receipt must say polling is pointless:\n%s", receipt)
	}
	if strings.Contains(receipt, "wait_seconds=20") {
		t.Errorf("wake-arm receipt must not suggest wait_seconds:\n%s", receipt)
	}

	t.Setenv(PostDelegPollEnv, "")
	off := (&ChatHarness{DelegateStatus: statusProbe("running", &waits)})
	off.pd.recordDelegation("task_w")
	if !strings.Contains(off.delegateReceipt("task_w"), "wait_seconds=20") {
		t.Error("the off arm's receipt must keep the existing poll advice")
	}
}

// Regression for the 2026-11-11 measurement that motivated the id set: a turn
// that delegates twice used to key the lever on the LAST id only, so a check
// for the first task matched nothing — `delegated=2`, two status calls in the
// trajectory, `post_deleg_polls=1`. Both hand-offs must be guarded, and the
// free check stays one per turn (the budget is per turn, not per task).
func TestPollLeverCoversEveryDelegatedTask(t *testing.T) {
	t.Setenv(PostDelegPollEnv, "wake")
	h := &ChatHarness{}
	h.pd.recordDelegation("task_a")
	h.pd.recordDelegation("task_b")

	if !h.pd.wasDelegated("task_a") || !h.pd.wasDelegated("task_b") {
		t.Fatalf("both hand-offs must be in the set, got %v", h.pd.taskIDs)
	}
	// Spend the free check on the second task, then check the first one: it must
	// still be counted and still be locked afterwards.
	if h.pd.blocksPoll("orchestrator_task_status", map[string]any{"task_id": "task_b"}) {
		t.Fatal("the first check of the turn must not be refused")
	}
	h.pd.notePoll("task_b")
	if h.pd.polls != 1 {
		t.Fatalf("the free check must be counted, polls=%d", h.pd.polls)
	}
	if !h.pd.blocksPoll("orchestrator_task_status", map[string]any{"task_id": "task_a"}) {
		t.Fatal("a check for the FIRST hand-off must be locked after the free one")
	}
	if h.pd.pollBlocked != 1 {
		t.Fatalf("the refusal must be counted, pollBlocked=%d", h.pd.pollBlocked)
	}
}

// A wake (follow-up) turn is a fresh harness, so it inherits nothing: without
// seeding the poll lever is blind exactly where the polling happens. Measured
// 2026-11-11 on the wake arm, the single status call of the run landed in
// `_fu1`, where `post_deleg_polls` read 0.
func TestWakeTurnSeesEarlierDelegationsButIsNotLockedOut(t *testing.T) {
	t.Setenv(PostDelegPollEnv, "wake")
	t.Setenv("VORTEX_POST_DELEG_LOCKOUT", "hard")
	h := &ChatHarness{}
	h.SeedDelegatedTasks([]string{"task_a", "task_b"})

	if !h.pd.wasDelegated("task_a") {
		t.Fatal("the wake turn must see the delegation it is continuing")
	}
	// Seeding must NOT turn on the per-turn lockout: a wake turn has to read the
	// result and write its own artifacts, so stripping its work tools would
	// break the very turn that consumes the result.
	if h.pd.blockedBy("execute_code") {
		t.Fatal("the work-tool lockout is per turn and must stay off in a wake turn")
	}
	if h.pd.delegatedHere {
		t.Fatal("seeding is not a delegation made by this harness")
	}
	if h.pd.delegations != 0 {
		t.Fatalf("seeding must not inflate the delegation count, got %d", h.pd.delegations)
	}
	// The lever itself must work here: one immediate check, the rest locked.
	if h.pd.blocksPoll("orchestrator_task_status", map[string]any{"task_id": "task_b"}) {
		t.Fatal("the first check in the wake turn must not be refused")
	}
	h.pd.notePoll("task_b")
	if !h.pd.blocksPoll("orchestrator_task_status", map[string]any{"task_id": "task_a"}) {
		t.Fatal("after the free check, a poll for any hand-off must be locked")
	}
}

// A status call for a task this session never handed off must be left alone:
// the lever is about the tasks we delegated, not about status in general.
func TestPollLeverIgnoresTasksThisSessionDidNotDelegate(t *testing.T) {
	t.Setenv(PostDelegPollEnv, "wake")
	h := &ChatHarness{}
	h.pd.recordDelegation("task_mine")
	h.pd.notePoll("task_mine")
	if h.pd.blocksPoll("orchestrator_task_status", map[string]any{"task_id": "task_someone_else"}) {
		t.Fatal("a poll for a foreign task must not be locked")
	}
	if h.pd.polls != 1 {
		t.Fatalf("a foreign poll must not be counted, polls=%d", h.pd.polls)
	}
}
