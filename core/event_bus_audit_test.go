package core

import (
	"sync"
	"testing"
)

// noopHandler is a simple EventHandler for testing.
func noopHandler(event AgentEvent) error { return nil }

// TestEventBus_SubscribeWithCancelAllNilReset verifies that when ALL handlers
// in a subscriber slice are tombstoned (nil), the slice is reset to empty.
// Without this, repeated subscribe/unsubscribe cycles would grow the slice
// with nil entries forever (unbounded memory growth).
func TestEventBus_SubscribeWithCancelAllNilReset(t *testing.T) {
	bus := NewEventBus()
	eventType := "test.event"

	// Subscribe and unsubscribe 20 handlers. Without the all-nil reset, the
	// slice would grow to 20 nil entries. With the fix, it should be reset
	// to empty when the last handler is tombstoned.
	var unsubs []func()
	for i := 0; i < 20; i++ {
		unsub := bus.SubscribeWithCancel(eventType, noopHandler)
		unsubs = append(unsubs, unsub)
	}

	// Unsubscribe all — the last tombstone triggers the all-nil reset.
	for _, unsub := range unsubs {
		unsub()
	}

	bus.mu.RLock()
	subs := bus.subscribers[eventType]
	bus.mu.RUnlock()

	if len(subs) != 0 {
		t.Fatalf("expected 0 entries after all unsubscribed + all-nil reset, got %d — slice is growing with nil entries unbounded", len(subs))
	}
}

// TestEventBus_AllNilResetPreservesLiveHandlers verifies that the all-nil
// reset only fires when ALL entries are tombstoned, not when some are live.
func TestEventBus_AllNilResetPreservesLiveHandlers(t *testing.T) {
	bus := NewEventBus()
	eventType := "test.event"

	// Subscribe 10 handlers, keep 3 live, unsubscribe 7.
	var liveUnsubs []func()
	var deadUnsubs []func()
	for i := 0; i < 10; i++ {
		unsub := bus.SubscribeWithCancel(eventType, noopHandler)
		if i < 3 {
			liveUnsubs = append(liveUnsubs, unsub)
		} else {
			deadUnsubs = append(deadUnsubs, unsub)
		}
	}

	// Unsubscribe the 7 dead ones — should NOT reset (3 live handlers remain).
	for _, unsub := range deadUnsubs {
		unsub()
	}

	bus.mu.RLock()
	subs := bus.subscribers[eventType]
	bus.mu.RUnlock()

	// Slice should still exist with 10 entries (7 nil + 3 live) because
	// not ALL are nil.
	liveCount := 0
	for _, h := range subs {
		if h != nil {
			liveCount++
		}
	}
	if liveCount != 3 {
		t.Fatalf("expected 3 live handlers, got %d", liveCount)
	}

	// Now unsubscribe the 3 live ones — ALL are nil → slice should be reset.
	for _, unsub := range liveUnsubs {
		unsub()
	}

	bus.mu.RLock()
	subs = bus.subscribers[eventType]
	bus.mu.RUnlock()

	if len(subs) != 0 {
		t.Fatalf("expected 0 entries after all unsubscribed (all-nil reset), got %d — slice is growing with nil entries unbounded", len(subs))
	}
}

// TestEventBus_PublishSkipsTombstoned verifies that Publish skips nil
// (tombstoned) handlers and only calls live ones.
func TestEventBus_PublishSkipsTombstoned(t *testing.T) {
	bus := NewEventBus()
	eventType := "test.event"

	var mu sync.Mutex
	callCount := 0

	// Subscribe 5 handlers, unsubscribe 3.
	var unsubs []func()
	for i := 0; i < 5; i++ {
		unsub := bus.SubscribeWithCancel(eventType, func(event AgentEvent) error {
			mu.Lock()
			callCount++
			mu.Unlock()
			return nil
		})
		unsubs = append(unsubs, unsub)
	}

	// Unsubscribe first 3.
	for i := 0; i < 3; i++ {
		unsubs[i]()
	}

	// Publish — only 2 live handlers should be called.
	bus.Publish(AgentEvent{EventType: eventType})

	mu.Lock()
	defer mu.Unlock()
	if callCount != 2 {
		t.Fatalf("expected 2 handler calls (live only), got %d — tombstoned handlers are being invoked", callCount)
	}
}
