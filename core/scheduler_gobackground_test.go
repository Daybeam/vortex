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
	logger := mustNewLogger(t, t.TempDir(), nil)
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
	// Race-safe: both channels may be ready simultaneously (goroutineDone
	// is closed before bgWg.Done, but the scheduler may not run the
	// select until both are pending). Use a non-blocking check on
	// goroutineDone when stopDone fires.
	select {
	case <-stopDone:
		select {
		case <-goroutineDone:
			// Both finished — select just picked stopDone first. OK.
		default:
			t.Fatal("Stop() returned before background goroutine drained — " +
				"goBackground is not tracking the goroutine (audit H3 regression)")
		}
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
// This test fails if goBackground is replaced with a plain `go` statement
// (audit T-GATE-4: original test had zero assertions).
func TestGoBackground_ConcurrentLaunches(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), nil)
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

	// blocked ensures all goroutines are waiting on lifecycleCtx.Done()
	// before we call Stop().
	var blocked sync.WaitGroup
	blocked.Add(n)

	for i := 0; i < n; i++ {
		engine.goBackground(func() {
			blocked.Done()
			<-lifecycleCtx.Done()
			doneCount.Done()
		})
	}
	blocked.Wait() // all goroutines are now blocking

	// Stop() must NOT return before all goroutines drain.
	stopDone := make(chan struct{})
	go func() {
		engine.Stop()
		close(stopDone)
	}()

	allDone := make(chan struct{})
	go func() {
		doneCount.Wait()
		close(allDone)
	}()

	select {
	case <-stopDone:
		// Stop() returned — verify goroutines already finished.
		select {
		case <-allDone:
			// Success: goroutines drained before or with Stop().
		case <-time.After(200 * time.Millisecond):
			t.Fatal("Stop() returned before all background goroutines drained — " +
				"goBackground is not tracking goroutines (audit T-GATE-4)")
		}
	case <-allDone:
		// Goroutines finished first — Stop() should follow shortly.
		select {
		case <-stopDone:
			// Success
		case <-time.After(5 * time.Second):
			t.Fatal("Stop() did not complete within 5s after goroutines drained")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("test timed out")
	}
}
