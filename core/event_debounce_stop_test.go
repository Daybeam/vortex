package core

import (
	"testing"
	"time"
)

// TestR2_SubscribeBatched_StopRemovesHandler is a regression test for
// audit R-2: SubscribeBatched must use SubscribeWithCancel so stop() can
// remove the handler from the EventBus.
//
// Before the fix, Subscribe was used, which left the handler closure in
// subscribers forever. Every Publish invoked the dead closure (capturing
// the ingest channel), causing a goroutine leak and wasted work.
// After the fix, stop() calls unsubscribe() which tombstones the handler.
//
// Reproduction: after stop(), publishing events should not invoke the
// subscriber handler (verified by checking the handler counter).
func TestR2_SubscribeBatched_StopRemovesHandler(t *testing.T) {
	bus := NewEventBus()

	// Track handler invocations.
	handlerInvoked := make(chan struct{}, 100)

	batches, stop := SubscribeBatched(bus, "r2_test_event", 10, 50*time.Millisecond)

	// Drain batches in background.
	go func() {
		for range batches {
			handlerInvoked <- struct{}{}
		}
	}()

	// Publish an event — should be received.
	bus.Publish(NewAgentEvent("task-1", "step-1", "r2_test_event", nil))

	select {
	case <-handlerInvoked:
		// Success: handler was invoked.
	case <-time.After(time.Second):
		t.Fatal("timeout: first event was not received")
	}

	// Stop the subscription.
	stop()

	// Give the stop goroutine time to tombstone the handler.
	time.Sleep(100 * time.Millisecond)

	// Publish more events — handler should NOT be invoked.
	bus.Publish(NewAgentEvent("task-1", "step-1", "r2_test_event", nil))
	bus.Publish(NewAgentEvent("task-1", "step-1", "r2_test_event", nil))

	// Wait a bit to see if the handler is incorrectly invoked.
	select {
	case <-handlerInvoked:
		t.Error("handler was invoked after stop() — R-2 fix not working (handler not removed from EventBus)")
	case <-time.After(200 * time.Millisecond):
		// Success: handler was not invoked.
	}
}
