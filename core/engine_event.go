package core

import "github.com/daybeam/vortex/core/obs"

// engine_event.go wires the existing EventBus (core/event_bus.go) into the
// scheduler's key state-transition points. It reuses the existing EventType
// constants from logger.go and AgentEvent from event_log.go — no new types.
//
// See docs/architecture/EVENT_DRIVEN_CALLBACK_DESIGN.md.

// publishEvent is a thin helper that publishes a structured AgentEvent to the
// global EventBus. Called at key scheduler state transitions so consumers
// (swarm provider, chat harness, audit loggers) receive real-time notifications
// instead of polling.
//
// Nil-safe: DefaultBus is always initialized (package-level var), so this
// never panics even if no subscribers are registered.
func (s *DirectedEngine) publishEvent(taskID, stepID string, et obs.EventType, payload map[string]any) {
	obs.DefaultBus.Publish(obs.NewAgentEvent(taskID, stepID, string(et), payload))
}
