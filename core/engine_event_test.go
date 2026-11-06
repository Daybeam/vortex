package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/core/obs"
)

func TestPublishEvent_DispatchesToBus(t *testing.T) {
	bus := obs.NewEventBus()
	received := make(chan obs.AgentEvent, 1)
	bus.Subscribe(string(obs.EventStepCompleted), func(e obs.AgentEvent) error {
		received <- e
		return nil
	})

	originalBus := obs.DefaultBus
	obs.DefaultBus = bus
	defer func() { obs.DefaultBus = originalBus }()

	engine := &DirectedEngine{}
	engine.publishEvent("task1", "step1", obs.EventStepCompleted, map[string]any{"conf": 0.9})

	select {
	case evt := <-received:
		if evt.TaskID != "task1" || evt.StepID != "step1" {
			t.Fatalf("unexpected event: %+v", evt)
		}
		if evt.EventType != string(obs.EventStepCompleted) {
			t.Fatalf("expected event_type %q, got %q", obs.EventStepCompleted, evt.EventType)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}
