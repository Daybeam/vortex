package core

// engine_event.go wires the existing EventBus (core/event_bus.go) into the
// scheduler's key state-transition points. It reuses the existing EventType
// constants from logger.go and AgentEvent from event_log.go — no new types.
//
// See docs/architecture/EVENT_DRIVEN_CALLBACK_DESIGN.md.

// eventPriority returns a priority level for an EventType.
// 0 = highest (interrupt consumer immediately), 2 = lowest (audit only).
// Consumers use this for priority routing and backpressure drop decisions.
func eventPriority(et EventType) int {
	switch et {
	case EventDecisionRequired:
		return 0
	case EventStepFailed, EventTaskFailed:
		return 0
	case EventStepCompleted, EventTaskCompleted:
		return 1
	case EventStepStarted:
		return 2
	default:
		return 1
	}
}

// publishEvent is a thin helper that publishes a structured AgentEvent to the
// global EventBus. Called at key scheduler state transitions so consumers
// (swarm provider, chat harness, audit loggers) receive real-time notifications
// instead of polling.
//
// Nil-safe: DefaultBus is always initialized (package-level var), so this
// never panics even if no subscribers are registered.
func (s *DirectedEngine) publishEvent(taskID, stepID string, et EventType, payload map[string]any) {
	DefaultBus.Publish(NewAgentEvent(taskID, stepID, string(et), payload))
}
