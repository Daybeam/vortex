package core

import (
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// SetAgents injects swarm agents into the engine. Called by external integrations only.
func (s *DirectedEngine) SetAgents(agents []interfaces.AutonomousProvider) {
	s.agents = agents
}

// GetAgents returns the current swarm agents. Called by external integrations only.
func (s *DirectedEngine) GetAgents() []interfaces.AutonomousProvider {
	return s.agents
}

// SetConvergenceMonitor injects a convergence monitor. Called by external integrations only.
func (s *DirectedEngine) SetConvergenceMonitor(cm interfaces.ConvergenceMonitor) {
	s.convMonitor = cm
}

// GetLogger returns the engine's logger. Called by external integrations only.
func (s *DirectedEngine) GetLogger() *Logger {
	return s.logger
}

// WithGraphs grants temporary access to the engine's task graphs. Called by external integrations only.
func (s *DirectedEngine) WithGraphs(fn func(graphs map[string]*schemas.TaskGraph)) {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	fn(s.graphs)
}

// GetRegistry returns the spawner's provider registry. Called by external integrations only.
func (s *Spawner) GetRegistry() *config.Registry { return s.registry }

// GetExpStore returns the spawner's experience store. Called by external integrations only.
func (s *Spawner) GetExpStore() store.IExperienceStore { return s.expStore }
