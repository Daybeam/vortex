package core

import (
	"context"
	"testing"
)

// Regression tests for M3 (cancellable context) and H2 (sync.Once idempotent
// Stop) in core/cookbook_syncer.go.
//
// Before M3, CookbookSyncer.SyncAll() used context.Background(), so calling
// Stop() (which only closed stopChan) could not cancel an in-flight sync —
// a slow repo fetch would keep running after shutdown. The fix added a
// cancellable context (s.ctx/s.cancel) that Stop() cancels before closing
// stopChan, and SyncAll() checks ctx.Err() in its repo loop.
//
// Before H2, Stop() called close(s.stopChan) unconditionally, so a second
// Stop() would panic on double-close. The fix wrapped the body in sync.Once.

// TestCookbookSyncer_Stop_CancelsContext verifies that Stop() cancels the
// syncer's internal context, so in-flight sync operations see ctx.Err() != nil
// and abort. This is the core invariant of the M3 fix.
func TestCookbookSyncer_Stop_CancelsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &CookbookSyncer{
		stopChan: make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}

	if s.ctx.Err() != nil {
		t.Fatal("context should not be cancelled before Stop()")
	}
	s.Stop()
	if s.ctx.Err() == nil {
		t.Fatal("context should be cancelled after Stop() — M3 fix not applied")
	}
}

// TestCookbookSyncer_Stop_Idempotent verifies that calling Stop() multiple
// times does not panic (double-close safety via sync.Once). This is the H2 fix.
func TestCookbookSyncer_Stop_Idempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &CookbookSyncer{
		stopChan: make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Stop() panicked on repeated call: %v", r)
		}
	}()
	s.Stop()
	s.Stop() // must not panic
	s.Stop() // third call for good measure
}

// TestCookbookSyncer_Stop_ClosesStopChan verifies that Stop() closes the
// stopChan, which is what signals the background loop to exit.
func TestCookbookSyncer_Stop_ClosesStopChan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &CookbookSyncer{
		stopChan: make(chan struct{}),
		ctx:      ctx,
		cancel:   cancel,
	}

	select {
	case <-s.stopChan:
		t.Fatal("stopChan should be open before Stop()")
	default:
		// good
	}
	s.Stop()
	select {
	case <-s.stopChan:
		// good — channel was closed
	default:
		t.Fatal("stopChan should be closed after Stop()")
	}
}
