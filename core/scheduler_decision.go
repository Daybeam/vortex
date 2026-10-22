package core

import (
	"github.com/daybeam/vortex/schemas"
)

func (s *DirectedEngine) handleOutput(
	graph *schemas.TaskGraph,
	step *schemas.Step,
	result *SpawnResult,
) {
	// L-N15 (2026-10-04): Guard against nil result. Although Spawn no longer
	// returns (nil, nil) after the L-N12 root-cause fix, defensive nil-check
	// prevents a nil-deref panic if a future caller bypasses Spawn.
	if result == nil {
		return
	}

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

	case output.Status == schemas.StatusCapabilityRequired || output.Status == schemas.StatusApprovalRequired || output.Status == schemas.StatusPartial || output.Status == schemas.StatusFailed || conf < s.registry.System.ConfidenceThreshold:
		// StatusFailed added (2026-10-20): previously, status="failed" with
		// high confidence fell through to default → handleDefaultFailure →
		// maybeBlock with ["skip", "abort"] — NO retry. Recoverable failures
		// (transient API errors, missing inputs) labeled "failed" by the model
		// were silently dropped. Now StatusFailed gets the full retry/refine
		// treatment via handleStepFailure, which offers ["skip", "retry",
		// "refine_and_retry", "abort"]. The decision maker can still choose
		// skip/abort for permanent failures — retry is an option, not forced.
		if verdict := s.handleStepFailure(graph, step, result); verdict == VerdictReturn {
			return
		}

	default:
		s.handleDefaultFailure(graph, step)
	}
}
