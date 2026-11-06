package replay

import (
	"path/filepath"
	"testing"
	"time"
)

// A run whose steps are still open is being written right now. Before the
// loader kept a clock, deriveStatus could only see terminal events and step
// tallies, so a live run was reported as "interrupted" (nothing finished yet)
// or "completed" (the first step finished) — the replay page told the user the
// task had stopped while it was still executing.

func statusNow() time.Time {
	return time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
}

func ago(now time.Time, seconds float64) float64 {
	return float64(now.UnixNano())/1e9 - seconds
}

func statusLoader(t *testing.T, taskID string, now time.Time, lines ...string) *Loader {
	t.Helper()
	root := t.TempDir()
	writeLogFile(t, root, now.Format("2006-01-02"), taskID, lines...)
	loader := NewLoader(filepath.Join(root, "logs"), filepath.Join(root, "tasks"))
	loader.now = func() time.Time { return now }
	return loader
}

func assertRunStatus(t *testing.T, loader *Loader, taskID, want string) {
	t.Helper()
	detail, err := loader.Detail(taskID)
	if err != nil {
		t.Fatalf("Detail(%s): %v", taskID, err)
	}
	if detail.Status != want {
		t.Fatalf("status = %q, want %q", detail.Status, want)
	}
	if detail.Settled {
		t.Fatal("settled = true for an unsettled run")
	}
}

// Link: deriveStatus had no in-flight branch; a live run fell through to
// StatusInterrupted.
func TestLiveRunWithOpenStepIsRunningNotInterrupted(t *testing.T) {
	const taskID = "task_live0001"
	now := statusNow()
	loader := statusLoader(t, taskID, now,
		line(taskID, "task_submitted", "", ago(now, 120), `{"step_count":2}`),
		line(taskID, "step_started", "s1", ago(now, 110), `{"role":"reader"}`),
		line(taskID, "EventCoreToolCall", "s1", ago(now, 60), `{"tool":"read_file"}`),
		line(taskID, "step_started", "s2", ago(now, 30), `{"role":"writer"}`),
	)
	assertRunStatus(t, loader, taskID, StatusRunning)
}

// Link: the same in-flight run took the `completed > 0` branch instead.
func TestLiveRunWithOneFinishedStepIsRunningNotCompleted(t *testing.T) {
	const taskID = "task_live0002"
	now := statusNow()
	loader := statusLoader(t, taskID, now,
		line(taskID, "task_submitted", "", ago(now, 300), `{"step_count":2}`),
		line(taskID, "step_started", "s1", ago(now, 290), `{"role":"reader"}`),
		line(taskID, "step_completed", "s1", ago(now, 200), `{"status":"ok"}`),
		line(taskID, "step_started", "s2", ago(now, 100), `{"role":"writer"}`),
		line(taskID, "EventCoreToolCall", "s2", ago(now, 10), `{"tool":"write_file"}`),
	)
	assertRunStatus(t, loader, taskID, StatusRunning)
}

// Link: a task still being decomposed has no step at all, so it also used to
// read as interrupted.
func TestDecomposingRunIsRunning(t *testing.T) {
	const taskID = "task_live0003"
	now := statusNow()
	loader := statusLoader(t, taskID, now,
		line(taskID, "task_submitted", "", ago(now, 5), `{"step_count":3}`),
		line(taskID, "smart_route", "", ago(now, 4), `{"task":"summarize the repo"}`),
	)
	assertRunStatus(t, loader, taskID, StatusRunning)
}

func TestStaleRunKeepsItsSettledStatuses(t *testing.T) {
	const taskID = "task_dead001"
	now := statusNow()

	// Killed mid-step: still interrupted, the fresh-tail rule must not turn
	// every unfinished run into a live one.
	stale := statusLoader(t, taskID, now,
		line(taskID, "task_submitted", "", ago(now, 7200), `{"step_count":2}`),
		line(taskID, "step_started", "s1", ago(now, 7190), `{"role":"reader"}`),
	)
	assertRunStatus(t, stale, taskID, StatusInterrupted)

	// All steps landed but the process died before task_completed: the tally
	// fallback still owns this case.
	completed := statusLoader(t, taskID, now,
		line(taskID, "task_submitted", "", ago(now, 7200), `{"step_count":1}`),
		line(taskID, "step_started", "s1", ago(now, 7190), `{"role":"reader"}`),
		line(taskID, "step_completed", "s1", ago(now, 7100), `{"status":"ok"}`),
	)
	assertRunStatus(t, completed, taskID, StatusCompleted)
}

// A run that logged its own terminal event is settled even when its newest
// event is inside the live window — the clock only breaks ties.
func TestTerminalEventBeatsFreshTail(t *testing.T) {
	const taskID = "task_done001"
	now := statusNow()
	loader := statusLoader(t, taskID, now,
		line(taskID, "task_submitted", "", ago(now, 120), `{"step_count":1}`),
		line(taskID, "step_started", "s1", ago(now, 110), `{"role":"reader"}`),
		line(taskID, "step_completed", "s1", ago(now, 30), `{"status":"ok"}`),
		line(taskID, "task_completed", "", ago(now, 2), `{}`),
	)
	detail, err := loader.Detail(taskID)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if detail.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q", detail.Status, StatusCompleted)
	}
	if !detail.Settled {
		t.Fatal("settled = false for a run that logged task_completed")
	}
}
