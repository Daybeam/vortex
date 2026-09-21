package core

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestSubscribeWithCancelTombstonesHandler verifies that after calling the
// unsubscribe function, Publish no longer invokes the handler. This is the
// regression test for audit H5: EventBus had no Unsubscribe, so handlers
// accumulated forever in the subscriber slice and Publish kept invoking dead
// closures on every event.
func TestSubscribeWithCancelTombstonesHandler(t *testing.T) {
	bus := NewEventBus()
	var calls int32

	unsub := bus.SubscribeWithCancel("test", func(e AgentEvent) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	// Publish before unsubscribe — handler should fire.
	bus.Publish(AgentEvent{EventType: "test"})
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 call before unsubscribe, got %d", got)
	}

	// Unsubscribe — tombstones the handler.
	unsub()

	// Publish after unsubscribe — handler must NOT fire.
	bus.Publish(AgentEvent{EventType: "test"})
	bus.Publish(AgentEvent{EventType: "test"})
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 call after unsubscribe (handler tombstoned), got %d", got)
	}
}

// TestSubscribeWithCancelIdempotent verifies that calling the unsubscribe
// function multiple times is a no-op (sync.Once guard).
func TestSubscribeWithCancelIdempotent(t *testing.T) {
	bus := NewEventBus()
	var calls int32

	unsub := bus.SubscribeWithCancel("test", func(e AgentEvent) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	unsub()
	unsub() // should not panic
	unsub() // should not panic

	bus.Publish(AgentEvent{EventType: "test"})
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("expected 0 calls after unsubscribe, got %d", got)
	}
}

// TestSubscribeBatchedStopUnsubscribes verifies that calling the stop function
// returned by SubscribeBatched actually unsubscribes from the EventBus, so
// subsequent Publish calls do not invoke the (now-dead) handler. Without this,
// the handler closure (which references the ingest channel and goroutine)
// would leak in the EventBus subscriber list forever (audit H5).
func TestSubscribeBatchedStopUnsubscribes(t *testing.T) {
	bus := NewEventBus()

	// Track whether the batch handler goroutine is still active by publishing
	// events after stop and checking that the ingest channel is no longer
	// being fed (i.e., the handler is tombstoned).
	batches, stop := SubscribeBatched(bus, "*", 1, 50*time.Millisecond)

	// Publish one event — should arrive in the batch channel.
	bus.Publish(AgentEvent{EventType: "test", TaskID: "t1"})
	select {
	case <-batches:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first batch")
	}

	// Stop the batch subscriber — should unsubscribe from the bus.
	stop()

	// Drain any remaining batch from the flushed goroutine.
	// After stop(), the goroutine flushes and closes the out channel, so we
	// must detect closure (ok==false) to avoid an infinite loop.
	for {
		select {
		case _, ok := <-batches:
			if !ok {
				goto drained // channel closed by goroutine exit
			}
		case <-time.After(200 * time.Millisecond):
			goto drained
		}
	}
drained:

	// Now publish more events. The handler should be tombstoned, so the
	// ingest channel is never fed and the goroutine has exited. We verify
	// this by checking that Publish returns quickly (no blocked send on a
	// full channel) and doesn't panic.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			bus.Publish(AgentEvent{EventType: "test", TaskID: "t1"})
		}
		close(done)
	}()
	select {
	case <-done:
		// good — all publishes completed without blocking
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked after stop — handler not unsubscribed (H5 leak)")
	}
}
