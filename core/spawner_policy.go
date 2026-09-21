package core

import (
	"github.com/daybeam/vortex/config"
)

// calculateMaxTurns computes the adaptive turn budget for a spawn request.
// It starts from the system static cap, then expands via IRT difficulty
// estimation and any accumulated TurnsBudgetBonus from resume decisions.
// IRT only ever EXPANDS the budget, never shrinks it below the static cap.
// Extracted from spawner.go per SPAWNER_REFACTORING_EXECUTION_PLAN.md Step 1.
func (s *Spawner) calculateMaxTurns(req *SpawnRequest, role *config.Role) int {
	maxTurns := 50
	if s.registry != nil && s.registry.System.MaxToolTurns > 0 {
		maxTurns = s.registry.System.MaxToolTurns
	}

	// IRT adaptive budget (ADDED 2026-09-13): difficulty-aware estimate
	// smoothed by historical turn data from the Experience Store.
	if s.irtEstimator != nil {
		historicalAvg := 0.0
		if s.expStore != nil {
			var sum float64
			var count int
			for _, p := range s.expStore.GetTaskPatternsSnapshot() {
				if p.TaskType == role.BaseCapability && p.AvgTurnsUsed > 0 {
					sum += p.AvgTurnsUsed
					count++
				}
			}
			if count > 0 {
				historicalAvg = sum / float64(count)
			}
		}
		if irtTurns := s.irtEstimator.CalculateAdaptiveTurnsWithTheta(req.Task, len(req.ContextRefs), historicalAvg, s.avgThetaForCapability(role.BaseCapability)); irtTurns > maxTurns {
			maxTurns = irtTurns
		}
	}

	maxTurns += req.TurnsBudgetBonus
	return maxTurns
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
