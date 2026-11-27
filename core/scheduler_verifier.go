package core

import (
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// resolveGraphVerifiers binds deployment-owned verifier policy to audit-enabled
// steps. SOPs and plans carry intent; provider/model names stay in config.
func (s *DirectedEngine) resolveGraphVerifiers(graph *schemas.TaskGraph) {
	if s.registry == nil {
		return
	}
	s.registry.Mu.RLock()
	system := s.registry.System
	s.registry.Mu.RUnlock()
	for _, step := range graph.Steps {
		if step == nil || (!step.RequireAudit && step.VerifierModel == "") {
			continue
		}
		source := "explicit_step"
		switch {
		case step.VerifierModel != "":
		case system.VerifierProvider != "" || system.VerifierModel != "":
			step.VerifierProvider = system.VerifierProvider
			step.VerifierModel = system.VerifierModel
			source = "system_config"
		default:
			step.VerifierProvider = s.registry.DefaultProviderName()
			source = "default_provider"
			graph.RecordSubstitution("verifier_default", "", step.VerifierProvider, "no explicit or configured verifier; using default provider", step.ID)
		}
		s.logger.Log(obs.EventVerifierResolved, graph.TaskID, step.ID, map[string]any{
			"source":   source,
			"provider": step.VerifierProvider,
			"model":    step.VerifierModel,
		})
	}
}
