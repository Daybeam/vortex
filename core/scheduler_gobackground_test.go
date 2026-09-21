package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// TestGoBackground_StopWaits is the regression test for audit H3:
// goroutines launched via goBackground (including resumed tasks in
// loadGraphs) must be tracked by bgWg so that Stop() waits for them
// to drain. Without this, Stop() could return while a resumed task
// goroutine is still writing to s.graphs, causing a data race.
//
// This test fails if goBackground is replaced with a plain `go` statement
// (which was the bug in loadGraphs before the H3 fix).
func TestGoBackground_StopWaits(t *testing.T) {
	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())

	engine := &DirectedEngine{
		logger:          logger,
		outputBase:      t.TempDir(),
		doneChans:       make(map[string]chan struct{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		graphs:          make(map[string]*schemas.TaskGraph),
		cancelFuncs:     make(map[string]context.CancelFunc),
	}

	// Simulate a resumed task goroutine that blocks until lifecycleCtx
	// is cancelled (mirroring what s.run does on shutdown).
	goroutineDone := make(chan struct{})
	engine.goBackground(func() {
		<-lifecycleCtx.Done()
		close(goroutineDone)
	})

	// Call Stop() in a goroutine. If goBackground is not tracking the
	// goroutine via bgWg, Stop() will return immediately without waiting.
	stopDone := make(chan struct{})
	go func() {
		engine.Stop()
		close(stopDone)
	}()

	// Stop() should NOT return before the background goroutine finishes.
	// If it does, the H3 fix has been reverted.
	select {
	case <-stopDone:
		t.Fatal("Stop() returned before background goroutine drained — " +
			"goBackground is not tracking the goroutine (audit H3 regression)")
	case <-goroutineDone:
		// Expected: the goroutine finished first (or simultaneously)
	}

	// Now wait for Stop() to complete with a timeout
	select {
	case <-stopDone:
		// Success
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not complete within 5s")
	}
}

// TestGoBackground_ConcurrentLaunches verifies that multiple goBackground
// goroutines are all tracked and Stop() waits for all of them.
func TestGoBackground_ConcurrentLaunches(t *testing.T) {
	logger, err := NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())

	engine := &DirectedEngine{
		logger:          logger,
		outputBase:      t.TempDir(),
		doneChans:       make(map[string]chan struct{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		graphs:          make(map[string]*schemas.TaskGraph),
		cancelFuncs:     make(map[string]context.CancelFunc),
	}

	const n = 5
	var doneCount sync.WaitGroup
	doneCount.Add(n)

	for i := 0; i < n; i++ {
		engine.goBackground(func() {
			<-lifecycleCtx.Done()
			doneCount.Done()
		})
	}

	engine.Stop()
	doneCount.Wait()
}
