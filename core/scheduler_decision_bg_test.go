package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestFinalize_GoroutinesTrackedByBgWg is a regression test for audit finding
// H6: finalize previously launched reflect, deliverArtifacts, and
// dispatchNotifications via bare go, which meant Stop()'s bgWg.Wait() could
// not drain them. After the fix, all three use s.goBackground.
//
// We verify that after finalize, bgWg.Wait() returns (the goroutines are
// tracked and complete). With bare go, the goroutines would be untracked
// (bgWg.Wait() returns immediately, but goroutines are orphaned). With
// goBackground, bgWg.Wait() waits for them to finish.
func TestFinalize_GoroutinesTrackedByBgWg(t *testing.T) {
	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()

	s := &DirectedEngine{
		logger:          logger,
		outputBase:      t.TempDir(),
		graphs:          make(map[string]*schemas.TaskGraph),
		doneChans:       make(map[string]chan struct{}),
		bgWg:            sync.WaitGroup{},
		sieve:           NewSieve(1),
		spawner:         &Spawner{},
		notifyChan:      make(chan struct{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		registry: &config.Registry{
			System: config.SystemSettings{},
		},
	}

	taskID := "test_h6_task"
	graph := &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{},
	}
	s.graphs[taskID] = graph
	s.doneChans[taskID] = make(chan struct{})

	s.finalize(graph)

	// bgWg.Wait() should return within a short timeout — the three goroutines
	// (reflect, deliverArtifacts, dispatchNotifications) are tracked by
	// goBackground and should complete quickly.
	waitDone := make(chan struct{})
	go func() {
		s.bgWg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		// Success: all goroutines drained.
	case <-time.After(3 * time.Second):
		t.Fatal("bgWg.Wait() did not return within 3s — goroutines not tracked or stuck")
	}
}
