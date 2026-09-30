package core

import (
	"context"

	"github.com/daybeam/vortex/config"
)

// calculateMaxTurns computes the adaptive turn budget for a spawn request.
// This is now a thin glue function: SignalCollector gathers data, PolicyDecider
// computes the decision, and this function applies it.
//
// The IRT formula, difficulty estimation, theta→strategy mapping, and skill
// injection logic have all moved to core/policy_decider.go (irtPolicyDecider).
// The data collection (expStore/capProfileStore queries) has moved to
// core/policy_signals.go (storeBackedSignalCollector).
//
// See docs/completed/2026-09-28/ for the refactoring rationale.
func (s *Spawner) calculateMaxTurns(ctx context.Context, req *SpawnRequest, role *config.Role) int {
	// audit LOGIC-NEW-1: nil guards for bare &Spawner{} in tests.
	// Production code always initializes these in NewSpawner, but 20+ test
	// files construct &Spawner{} directly — a nil interface call would panic.
	if s.signalCollector == nil || s.policyDecider == nil {
		// Safe fallback: 50 matches NewIRTBudgetEstimator's default.
		if s.registry != nil && s.registry.System.MaxToolTurns > 0 {
			return s.registry.System.MaxToolTurns
		}
		return 50
	}
	signals := s.signalCollector.Collect(ctx, req, role)
	decision := s.policyDecider.Decide(signals, req, role)

	// Apply injected skills from the decision
	req.AdditionalSkills = append(req.AdditionalSkills, decision.InjectedSkills...)

	return decision.MaxTurns + req.TurnsBudgetBonus
}

// GetEffectiveHandoffThreshold resolves the actual threshold used for
// upstream context injection. It respects an explicit static threshold
// if set; otherwise it derives adaptively from the active model's
// MaxContextWindow (defaulting to 15% of the model's window at ~4 bytes/token).
// If no model window is available, it falls back to a safe 32KB default.
// Moved from spawner.go during god-class split.
func (s *Spawner) GetEffectiveHandoffThreshold(pCfg *config.ProviderConfig) int {
	effectiveThreshold := s.refBasedHandoffThreshold
	if effectiveThreshold <= 0 && pCfg != nil {
		maxCtx := pCfg.MaxContextWindow
		if maxCtx <= 0 && s.modelRegistry != nil && pCfg.Model != "" {
			maxCtx = s.modelRegistry.GetCapabilities(pCfg.Model).MaxContextWindow
		}
		if maxCtx > 0 {
			effectiveThreshold = int(float64(maxCtx) * 0.15 * 4)
		}
	}
	if effectiveThreshold <= 0 {
		effectiveThreshold = 32768
	}
	return effectiveThreshold
}
