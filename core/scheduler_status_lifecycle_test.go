package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestApprovePlan_GoroutineTrackedAndContextDerived is a regression test for
// audit finding C5: ApprovePlan previously used context.Background() (so
// Stop() couldn't cancel the task) and bare go (so Stop()'s bgWg.Wait()
// couldn't drain the goroutine). After the fix, it uses s.lifecycleCtx and
// s.goBackground.
//
// We verify both aspects:
//  1. After ApprovePlan, bgWg.Wait() does NOT return immediately — the
//     goroutine is tracked (goBackground) and still running (swarmWatchdog
//     blocks on ctx.Done()).
//  2. After cancelling lifecycleCtx, bgWg.Wait() returns within 2s — the
//     context is derived from lifecycleCtx, so swarmWatchdog exits.
//
// If the bug is present (bare go + context.Background()), step 1 fails
// (bgWg.Wait() returns immediately because the goroutine isn't tracked).
// If only the context bug is present, step 2 fails (bgWg.Wait() doesn't
// return because swarmWatchdog's context isn't cancelled).
func TestApprovePlan_GoroutineTrackedAndContextDerived(t *testing.T) {
	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())

	s := &DirectedEngine{
		logger:      logger,
		outputBase:  t.TempDir(),
		graphs:      make(map[string]*schemas.TaskGraph),
		doneChans:   make(map[string]chan struct{}),
		cancelFuncs: make(map[string]context.CancelFunc),
		bgWg:        sync.WaitGroup{},
		UseSwarm:    true,
		notifyChan:  make(chan struct{}),
		registry: &config.Registry{
			System: config.SystemSettings{
				SwarmFallbackDelay: 60, // long delay so swarmWatchdog blocks on ctx.Done()
			},
		},
	}
	s.lifecycleCtx = lifecycleCtx
	s.lifecycleCancel = lifecycleCancel

	taskID := "test_c5_task"
	s.graphs[taskID] = &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphPendingReview,
		Steps:  map[string]*schemas.Step{},
	}

	if err := s.ApprovePlan(taskID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}

	// Step 1: bgWg.Wait() should NOT return immediately — the goroutine
	// is tracked by goBackground and swarmWatchdog is still blocking.
	waitDone := make(chan struct{})
	go func() {
		s.bgWg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("bgWg.Wait() returned immediately — goroutine not tracked (bare go bug)")
	case <-time.After(200 * time.Millisecond):
		// Good: goroutine is still running and tracked.
	}

	// Step 2: cancel lifecycleCtx — swarmWatchdog's ctx (derived from
	// lifecycleCtx) should be cancelled, causing it to exit.
	lifecycleCancel()

	select {
	case <-waitDone:
		// Success: goroutine exited and bgWg drained.
	case <-time.After(2 * time.Second):
		t.Fatal("bgWg.Wait() did not return within 2s after lifecycleCancel — context not derived from lifecycleCtx")
	}
}
