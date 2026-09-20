package core

import (
	"sync"
	"testing"
)

// panickingHandler is an EventHandler that always panics.
func panickingHandler(event AgentEvent) error {
	panic("intentional test panic")
}

// TestEventBus_PublishRecoversFromPanic is the regression test for audit M5:
// Publish must recover from handler panics so one panicking handler cannot
// crash the orchestrator. Before the fix (safeEventHandler wrapper with
// recover()), a panicking handler would propagate up through Publish and
// crash the calling goroutine.
//
// We verify this by subscribing a panicking handler alongside a normal
// handler, then publishing an event. The normal handler must still be
// called (panic was recovered, not propagated), and Publish must return
// normally (not panic).
func TestEventBus_PublishRecoversFromPanic(t *testing.T) {
	bus := NewEventBus()
	eventType := "test.panic"

	var mu sync.Mutex
	normalCalled := false

	// Subscribe a panicking handler first.
	bus.Subscribe(eventType, panickingHandler)
	// Subscribe a normal handler after it.
	bus.Subscribe(eventType, func(event AgentEvent) error {
		mu.Lock()
		normalCalled = true
		mu.Unlock()
		return nil
	})

	// Publish must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Publish panicked despite safeEventHandler recovery: %v", r)
		}
	}()
	bus.Publish(AgentEvent{EventType: eventType})

	mu.Lock()
	defer mu.Unlock()
	if !normalCalled {
		t.Fatal("normal handler was not called — panic from earlier handler propagated and aborted Publish")
	}
}

// TestEventBus_PublishRecoversFromWildcardPanic verifies that panic recovery
// also covers wildcard ("*") handlers, not just specific-event handlers.
func TestEventBus_PublishRecoversFromWildcardPanic(t *testing.T) {
	bus := NewEventBus()
	eventType := "test.wildcard_panic"

	var mu sync.Mutex
	normalCalled := false

	// Wildcard handler that panics.
	bus.Subscribe("*", panickingHandler)
	// Specific normal handler.
	bus.Subscribe(eventType, func(event AgentEvent) error {
		mu.Lock()
		normalCalled = true
		mu.Unlock()
		return nil
	})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Publish panicked from wildcard handler: %v", r)
		}
	}()
	bus.Publish(AgentEvent{EventType: eventType})

	mu.Lock()
	defer mu.Unlock()
	if !normalCalled {
		t.Fatal("normal handler was not called — wildcard panic propagated")
	}
}
