package core

import (
	"github.com/daybeam/vortex/schemas"
)

func (s *DirectedEngine) handleOutput(
	graph *schemas.TaskGraph,
	step *schemas.Step,
	result *SpawnResult,
) {
	// ── Phase 0.1: Token Budget Deduction (ADDED 2026-08-30) ─────────────
	if result != nil && result.Output.Usage != nil && result.Output.Usage.TotalTokens > 0 && s.budgetGuard != nil {
		// FIX (2026-08-31): DeductBudget mutates graph.TokensUsed/BudgetPaused,
		// graph-level counters shared across every concurrently-running step
		// of this same graph -- see the matching fix + comment on the
		// CheckBudget call site in executeStep above. Take a short write
		// lock around just this mutation.
		s.Mu.Lock()
		s.budgetGuard.DeductBudget(graph, int64(result.Output.Usage.TotalTokens))
		s.Mu.Unlock()
	}

	output := result.Output
	conf := output.Confidence
	s.Mu.Lock()
	step.Confidence = &conf
	step.MissingCtx = output.MissingContext
	step.StatesVisited = result.StatesVisited // PGPO (ADDED 2026-09-08)
	s.Mu.Unlock()

	switch {
	case output.Status == schemas.StatusOK && conf >= s.registry.System.ConfidenceThreshold:
		s.handleStepSuccess(graph, step, result)

	case output.Status == schemas.StatusDelegationRequired:
		s.handleDelegationRequired(graph, step, output)

	case output.Status == schemas.StatusCapabilityRequired || output.Status == schemas.StatusPartial || conf < s.registry.System.ConfidenceThreshold:
		if verdict := s.handleStepFailure(graph, step, result); verdict == VerdictReturn {
			return
		}

	default:
		s.handleDefaultFailure(graph, step)
	}
}
