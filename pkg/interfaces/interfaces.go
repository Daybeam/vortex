package interfaces

import (
	"context"
	"github.com/daybeam/vortex/pkg/types"
	"github.com/daybeam/vortex/schemas"
)

// Provider is the interface all AI backends must satisfy.
type Provider interface {
	Complete(ctx context.Context, req schemas.CompleteRequest) (*schemas.ProviderResponse, error)
	StreamComplete(ctx context.Context, req schemas.CompleteRequest, onChunk func(text string) error) (*schemas.ProviderResponse, error)
	Embed(ctx context.Context, text string) ([]float32, error)
	CountTokens(ctx context.Context, text string) (int, error)
	Name() string
}

// SignalFieldInterface defines the contract for interaction with the SignalField.
type SignalFieldInterface interface {
	Sensing(layer types.SignalLayer, limit int) []string
	SensingGlobal(limit int, agentLoad float64) []string
	Deposit(layer types.SignalLayer, taskID string, value float64)
	GetEffectiveSignal(taskID string, agentLoad float64) float64
}

// TaskHubInterface provides access to the task environment for autonomous workers.
type TaskHubInterface interface {
	GetSignalField() SignalFieldInterface
	ClaimTask(taskID string) bool
	ExecuteTask(ctx context.Context, taskID string) (*schemas.SubagentOutput, error)
	GetNotifyChan() chan struct{}

	// EvoX Swarm Affinity
	GetRoleAffinity(roleID, taskID string) float64
}

// AutonomousProvider is the interface for swarm agents.
type AutonomousProvider interface {
	Provider
	// Start starts the autonomous behavior loop.
	Start(ctx context.Context, hub TaskHubInterface) error
	// Stop stops the autonomous behavior loop.
	Stop() error
}

// ConvergenceMonitor is the interface for swarm convergence monitoring.
// The concrete implementation lives in the enterprise repo; the core
// holds only this interface so it can lifecycle-manage it via Start/Stop.
type ConvergenceMonitor interface {
	Start(ctx context.Context)
	Stop()
}
