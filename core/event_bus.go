package core

import (
	"log"
	"sync"
)

// EventHandler is a function that processes an AgentEvent.
type EventHandler func(event AgentEvent) error

// EventBus is a Cordis-style pub/sub bus for decoupling core scheduler from auxiliary systems.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[string][]EventHandler
}

// NewEventBus creates a new initialized EventBus.
func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[string][]EventHandler),
	}
}

// Subscribe adds a handler for a specific event type. Use "*" for all events.
func (b *EventBus) Subscribe(eventType string, handler EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[eventType] = append(b.subscribers[eventType], handler)
}

// SubscribeWithCancel registers a handler and returns an unsubscribe function.
// Calling it tombstones the handler (Publish skips nil entries) so the handler
// and anything it closes over (channels, goroutines) can be garbage-collected.
// This is the non-breaking counterpart to Subscribe for callers that need to
// tear down a subscription — without it, handlers accumulate forever in the
// subscriber slice and Publish keeps invoking dead closures on every event
// (audit H5). Idempotent: calling the returned func more than once is a no-op.
// After tombstoning, if ALL entries in the slice are nil, the slice is reset
// to empty to prevent unbounded nil growth from repeated subscribe/unsubscribe
// cycles. (We cannot compact by shifting because existing unsubscribe closures
// capture fixed indices — shifting would invalidate them.)
func (b *EventBus) SubscribeWithCancel(eventType string, handler EventHandler) (unsubscribe func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[eventType] = append(b.subscribers[eventType], handler)
	idx := len(b.subscribers[eventType]) - 1
	var once sync.Once
	unsubscribe = func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			subs := b.subscribers[eventType]
			if idx < len(subs) {
				subs[idx] = nil // tombstone; Publish skips nil
			}
			// If ALL entries are nil, reset the slice to prevent unbounded
			// nil growth. Safe because there are no live handlers to preserve.
			allNil := len(subs) > 0
			for _, h := range subs {
				if h != nil {
					allNil = false
					break
				}
			}
			if allNil {
				b.subscribers[eventType] = nil
			}
		})
	}
	return unsubscribe
}

// Publish dispatches an event to all relevant subscribers.
// Delivery is synchronous to ensure ordering for state-tracking plugins.
func (b *EventBus) Publish(event AgentEvent) {
	b.mu.RLock()
	handlers := b.subscribers[event.EventType]
	allHandlers := b.subscribers["*"]
	b.mu.RUnlock()

	// Execute specific handlers (skip tombstoned nil entries — audit H5)
	for _, h := range handlers {
		if h == nil {
			continue
		}
		safeEventHandler(h, event)
	}

	// Execute wildcard handlers
	for _, h := range allHandlers {
		if h == nil {
			continue
		}
		safeEventHandler(h, event)
	}
}

// safeEventHandler invokes a handler with panic recovery so one panicking
// handler cannot crash the publisher (audit M5).
func safeEventHandler(h EventHandler, event AgentEvent) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("WARN: event_bus: handler panicked: %v", r)
		}
	}()
	_ = h(event)
}

// DefaultBus is a shared global event bus instance.
var DefaultBus = NewEventBus()
