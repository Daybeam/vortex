package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for H1: fire-and-forget goroutines tracked via WaitGroup.
//
// Before the fix, DirectedEngine spawned background goroutines via bare `go
// s.foo()` for embedding updates, node folding, and reflection. Stop() only
// cancelled the lifecycle context but never waited for these goroutines to
// drain, so a process could exit while a goroutine was mid-write, corrupting
// on-disk state. The fix added bgWg sync.WaitGroup + goBackground() helper;
// Stop() now calls s.bgWg.Wait() after cancelling lifecycleCtx.

// TestDirectedEngine_GoBackground_StopWaitsForGoroutine verifies that Stop()
// blocks until a goroutine launched via goBackground() has fully returned.
// This is the core invariant of the H1 fix: no background work is left
// in-flight after Stop() returns.
func TestDirectedEngine_GoBackground_StopWaitsForGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &DirectedEngine{
		lifecycleCtx:    ctx,
		lifecycleCancel: cancel,
	}

	var completed int32
	ready := make(chan struct{})

	s.goBackground(func() {
		// Signal that the goroutine is running, then sleep long enough
		// that Stop() would definitely return before us if the WaitGroup
		// weren't being honored.
		close(ready)
		time.Sleep(100 * time.Millisecond)
		atomic.StoreInt32(&completed, 1)
	})

	<-ready // ensure goroutine has started
	s.Stop()

	if atomic.LoadInt32(&completed) != 1 {
		t.Fatal("Stop() returned before the goBackground goroutine completed — bgWg.Wait() is not being called or the goroutine was not tracked")
	}
}

// TestDirectedEngine_GoBackground_MultipleGoroutinesAllDrained verifies that
// Stop() waits for ALL tracked goroutines, not just the last one launched.
func TestDirectedEngine_GoBackground_MultipleGoroutinesAllDrained(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &DirectedEngine{
		lifecycleCtx:    ctx,
		lifecycleCancel: cancel,
	}

	const n = 5
	var completed int32
	var ready sync.WaitGroup
	ready.Add(n)

	for i := 0; i < n; i++ {
		s.goBackground(func() {
			ready.Done()
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&completed, 1)
		})
	}

	ready.Wait() // all goroutines started
	s.Stop()

	if got := atomic.LoadInt32(&completed); got != n {
		t.Fatalf("expected all %d goroutines to complete before Stop() returned, got %d", n, got)
	}
}

// TestDirectedEngine_Stop_NoTrackedGoroutines_NoBlock verifies that Stop()
// returns promptly when no background goroutines are tracked (bgWg.Wait()
// on a zero-count WaitGroup is an instant no-op).
func TestDirectedEngine_Stop_NoTrackedGoroutines_NoBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &DirectedEngine{
		lifecycleCtx:    ctx,
		lifecycleCancel: cancel,
	}

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
		// good — Stop() returned immediately
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() blocked for >2s with no tracked goroutines — bgWg.Wait() may be deadlocking")
	}
}

// TestDirectedEngine_Stop_LifecycleContextCancelled verifies that Stop()
// cancels the lifecycle context, which is what signals background goroutines
// to exit promptly (they should be checking lifecycleCtx).
func TestDirectedEngine_Stop_LifecycleContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &DirectedEngine{
		lifecycleCtx:    ctx,
		lifecycleCancel: cancel,
	}

	if ctx.Err() != nil {
		t.Fatal("lifecycle context should not be cancelled before Stop()")
	}
	s.Stop()
	if ctx.Err() == nil {
		t.Fatal("lifecycle context should be cancelled after Stop()")
	}
}
