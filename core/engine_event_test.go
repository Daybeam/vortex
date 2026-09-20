package core

import (
	"testing"
	"time"
)

func TestEventPriority(t *testing.T) {
	tests := []struct {
		et   EventType
		want int
	}{
		{EventDecisionRequired, 0},
		{EventStepFailed, 0},
		{EventTaskFailed, 0},
		{EventStepCompleted, 1},
		{EventTaskCompleted, 1},
		{EventStepStarted, 2},
		{EventStepPartial, 1},
	}
	for _, tt := range tests {
		got := eventPriority(tt.et)
		if got != tt.want {
			t.Fatalf("eventPriority(%q) = %d, want %d", tt.et, got, tt.want)
		}
	}
}

func TestPublishEvent_DispatchesToBus(t *testing.T) {
	bus := NewEventBus()
	received := make(chan AgentEvent, 1)
	bus.Subscribe(string(EventStepCompleted), func(e AgentEvent) error {
		received <- e
		return nil
	})

	originalBus := DefaultBus
	DefaultBus = bus
	defer func() { DefaultBus = originalBus }()

	engine := &DirectedEngine{}
	engine.publishEvent("task1", "step1", EventStepCompleted, map[string]any{"conf": 0.9})

	select {
	case evt := <-received:
		if evt.TaskID != "task1" || evt.StepID != "step1" {
			t.Fatalf("unexpected event: %+v", evt)
		}
		if evt.EventType != string(EventStepCompleted) {
			t.Fatalf("expected event_type %q, got %q", EventStepCompleted, evt.EventType)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestSubscribeBatched_DebounceAndPriority(t *testing.T) {
	bus := NewEventBus()
	batches, stop := SubscribeBatched(bus, "*", 10, 50*time.Millisecond)
	defer stop()

	bus.Publish(NewAgentEvent("t1", "s1", string(EventStepCompleted), nil))
	bus.Publish(NewAgentEvent("t1", "s2", string(EventStepStarted), nil))
	bus.Publish(NewAgentEvent("t1", "s3", string(EventDecisionRequired), nil))

	select {
	case batch := <-batches:
		if len(batch) < 2 {
			t.Fatalf("expected at least 2 events in batch, got %d", len(batch))
		}
		if batch[0].EventType != string(EventDecisionRequired) {
			t.Fatalf("expected first event to be highest priority (decision_required), got %q", batch[0].EventType)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batch")
	}
}

func TestSubscribeBatched_BatchSizeFlush(t *testing.T) {
	bus := NewEventBus()
	batches, stop := SubscribeBatched(bus, "*", 3, 10*time.Second)
	defer stop()

	for i := 0; i < 3; i++ {
		bus.Publish(NewAgentEvent("t1", "s", string(EventStepCompleted), nil))
	}

	select {
	case batch := <-batches:
		if len(batch) != 3 {
			t.Fatalf("expected batch of 3, got %d", len(batch))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batch flush on size")
	}
}
