package core

import (
	"sync"
	"testing"
	"time"
)

// TestC1_EventBus_ConcurrentPublishUnsubscribe is a regression test for
// audit C-1: EventBus.Publish must deep-copy the subscriber slice under RLock
// so that iteration is safe after unlock, even if SubscribeWithCancel
// tombstones entries concurrently.
//
// Before the fix, Publish used a slice header copy which still shared the
// backing array with subscribers, racing with SubscribeWithCancel's
// tombstone write (subs[idx] = nil).
// After the fix, elements are deep-copied under RLock.
//
// Reproduction: run concurrent Publish + Unsubscribe and verify no panic.
// Run with `go test -race` to detect the data race.
func TestC1_EventBus_ConcurrentPublishUnsubscribe(t *testing.T) {
	bus := NewEventBus()

	const numHandlers = 100
	unsubscribes := make([]func(), numHandlers)

	// Subscribe many handlers.
	for i := 0; i < numHandlers; i++ {
		unsubscribes[i] = bus.SubscribeWithCancel("concurrent_event", func(event AgentEvent) error {
			return nil
		})
	}

	var wg sync.WaitGroup
	const numGoroutines = 50

	// Concurrent publishers.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				bus.Publish(NewAgentEvent("task-1", "step-1", "concurrent_event", nil))
			}
		}()
	}

	// Concurrent unsubscribers.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < numHandlers; j++ {
				unsubscribes[j]()
			}
		}(i)
	}

	// Concurrent new subscribers.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				unsub := bus.SubscribeWithCancel("concurrent_event", func(event AgentEvent) error {
					return nil
				})
				_ = unsub
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success: no panic or deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: concurrent Publish/Unsubscribe deadlocked")
	}
}

// TestC1_EventBus_TombstonedHandlerSkipped verifies that tombstoned (nil)
// handlers are skipped during Publish, which is the companion to the C-1 fix.
func TestC1_EventBus_TombstonedHandlerSkipped(t *testing.T) {
	bus := NewEventBus()

	callCount := 0
	unsub := bus.SubscribeWithCancel("test_skip", func(event AgentEvent) error {
		callCount++
		return nil
	})

	// Tombstone the handler.
	unsub()

	bus.Publish(NewAgentEvent("task-1", "step-1", "test_skip", nil))

	if callCount != 0 {
		t.Errorf("tombstoned handler should be skipped, but was called %d times", callCount)
	}
}
