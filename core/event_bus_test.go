package core

import (
	"sync"
	"testing"
	"time"
)

func TestEventBus_PubSub(t *testing.T) {
	bus := NewEventBus()

	var wg sync.WaitGroup
	wg.Add(2)

	receivedSpecific := false
	receivedWildcard := false

	bus.Subscribe("test_event", func(event AgentEvent) error {
		if event.EventType == "test_event" {
			receivedSpecific = true
		}
		wg.Done()
		return nil
	})

	bus.Subscribe("*", func(event AgentEvent) error {
		receivedWildcard = true
		wg.Done()
		return nil
	})

	event := NewAgentEvent("task-1", "step-1", "test_event", map[string]any{"foo": "bar"})
	bus.Publish(event)

	// Use a timeout to avoid hanging if the test fails
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		if !receivedSpecific {
			t.Errorf("expected specific handler to be called")
		}
		if !receivedWildcard {
			t.Errorf("expected wildcard handler to be called")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("timeout waiting for handlers")
	}
}

func TestEventBus_Sync(t *testing.T) {
	bus := NewEventBus()

	start := time.Now()

	bus.Subscribe("slow_event", func(event AgentEvent) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	})

	event := NewAgentEvent("task-1", "step-1", "slow_event", nil)
	bus.Publish(event)

	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("Publish returned too early (%v), should be sync", elapsed)
	}
}
