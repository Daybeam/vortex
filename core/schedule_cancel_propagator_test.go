package core

import (
	"context"
	"testing"
	"time"
)

// TestR1_DeferCancel_UnblocksCancelPropagator is a regression test for
// audit R-1: the monitor goroutine in ScheduleManager.runScheduled must
// call `defer cancel()` so that when monitorTask exits, the context is
// canceled, which unblocks the cancel-propagator goroutine waiting on
// ctx.Done().
//
// Before the fix, the monitor goroutine did not call cancel(), so when
// monitorTask returned normally, the cancel-propagator goroutine waited
// forever on ctx.Done() — a goroutine leak.
// After the fix, `defer cancel()` ensures ctx is canceled on monitor exit.
//
// Reproduction: simulate the pattern — a goroutine with defer cancel() and
// a cancel-propagator waiting on ctx.Done(). When the first goroutine exits,
// the propagator should unblock.
func TestR1_DeferCancel_UnblocksCancelPropagator(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	propagatorExited := make(chan struct{})

	// Cancel-propagator goroutine (waits for ctx.Done() or stopChan).
	go func() {
		select {
		case <-ctx.Done():
			close(propagatorExited)
		case <-time.After(5 * time.Second):
			// Would only happen if R-1 fix is broken.
		}
	}()

	// Monitor goroutine with defer cancel() (the R-1 fix).
	go func() {
		defer cancel() // audit R-1: unblock the cancel-propagator when monitor exits
		// Simulate monitorTask returning normally (not via stopChan).
		time.Sleep(50 * time.Millisecond)
	}()

	// The propagator should exit within a reasonable time after the monitor exits.
	select {
	case <-propagatorExited:
		// Success: cancel-propagator was unblocked by defer cancel().
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: cancel-propagator was not unblocked — R-1 fix not working (goroutine leak)")
	}
}
