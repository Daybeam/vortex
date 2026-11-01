package obs

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
