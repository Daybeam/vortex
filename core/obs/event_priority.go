package obs

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
