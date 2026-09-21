package types

// SignalLayer defines the priority levels for tasks in the swarm.
type SignalLayer string

const (
	LayerCritical      SignalLayer = "critical"
	LayerDependency    SignalLayer = "dependency"
	LayerOpportunistic SignalLayer = "opportunistic"
	LayerExploration   SignalLayer = "exploration"
)
