package core

import (
	"context"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestCancelTask_RunLoopExitsPromptly_NoOwnGoroutineForStagedWorkspace
// verifies the second P0 cancellation risk point flagged in
// docs/completed/2026-09-14/COST_GOVERNANCE_ACTIVE_ALERT_AND_CONTROL.md §3.1/§6:
// "StagedWorkspace 在 scheduler_dag.go:356 起的后台 goroutine 是否绑 task ctx?"
//
// FINDING (2026-09-17): there is no separate StagedWorkspace goroutine to
// verify. core/staged_workspace.go's methods (TakePreStepSnapshot,
// RecordPostStepSnapshot, Rollback, AppendJournalEntry, CleanupStaging) are
// all synchronous and are only ever called inline from within executeStep,
// on the same per-step goroutine (*DirectedEngine).run's Phase 2 loop
// spawns. That goroutine's ctx is exactly the ctx CancelTask cancels --
// traced end to end: SubmitWithSessionIR/ApprovePlan do
// `ctx, cancel := context.WithCancel(...); s.cancelFuncs[taskID] = cancel;
// go s.run(ctx, taskID)`, and CancelTask calls that same cancel(). So the
// real, checkable claim this doc's own concern reduces to is: does run()'s
// own loop exit promptly once CancelTask fires? This test verifies that
// directly against the branch that would otherwise sleep on a 200ms poll
// indefinitely (no ready steps, graph not terminal) -- with
// System.StagingEnabled=true so this is verified against exactly the
// registry state the design doc's concern was raised about, not one where
// StagedWorkspace would never have been invoked anyway.
//
// CAVEAT (documented, not fixed): because StagedWorkspace's scanning
// methods (scanWorkspace/snapshotAll) do not poll ctx mid-scan, a scan
// already in progress when CancelTask fires runs to completion rather than
// aborting instantly -- this is best-effort cancellation, consistent with
// every other synchronous, non-ctx-aware helper in this codebase. It is
// not a goroutine leak: once the current step goroutine returns, nothing
// staging-related is left running, because nothing staging-related ever
// had its own goroutine to begin with.
func TestCancelTask_RunLoopExitsPromptly_NoOwnGoroutineForStagedWorkspace(t *testing.T) {
	s, reg := newAbortTestEngine(t)
	defer s.Stop()
	reg.System.StagingEnabled = true

	graph := &schemas.TaskGraph{
		TaskID: "task-staging-cancel-1",
		Steps: map[string]*schemas.Step{
			// A single StepRunning step: ReadySteps() skips it (not
			// StepPending), and IsTerminal() is false for StepRunning, so
			// run() lands in the "no ready steps, not terminal" branch --
			// the exact select{} that must observe ctx.Done() rather than
			// only its 200ms timer.
			"s1": {ID: "s1", Status: schemas.StepRunning},
		},
		Status: schemas.GraphRunning,
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.cancelFuncs[graph.TaskID] = cancel
	s.Mu.Unlock()

	runDone := make(chan struct{})
	go func() {
		s.run(ctx, graph.TaskID)
		close(runDone)
	}()

	// Give run() a moment to enter its poll loop before cancelling.
	time.Sleep(20 * time.Millisecond)

	if err := s.CancelTask(graph.TaskID); err != nil {
		t.Fatalf("CancelTask failed: %v", err)
	}

	select {
	case <-runDone:
		// run() genuinely exited -- cascade cancel reaches the goroutine
		// that any StagedWorkspace call would execute synchronously
		// within, confirming there is no separate, independently-lived
		// staging goroutine left unaccounted for.
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not exit within 2s of CancelTask -- cascade cancel broken for the goroutine StagedWorkspace's synchronous calls execute within")
	}

	s.Mu.RLock()
	status := graph.Status
	s.Mu.RUnlock()
	if status != schemas.GraphCancelled {
		t.Fatalf("expected graph status %s after CancelTask, got %s", schemas.GraphCancelled, status)
	}
}
