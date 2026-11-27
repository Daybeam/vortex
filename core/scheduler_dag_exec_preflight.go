package core

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// precheckStepBudget preserves the short RLock/Lock boundaries around budget
// reads and mutations. addDecision must never be called while s.Mu is held.
func (s *DirectedEngine) precheckStepBudget(graph *schemas.TaskGraph, step *schemas.Step) bool {
	if s.budgetGuard == nil {
		return true
	}
	s.Mu.RLock()
	budgetErr := s.budgetGuard.CheckBudget(graph)
	s.Mu.RUnlock()
	if budgetErr != nil {
		s.Mu.Lock()
		graph.BudgetPaused = true
		step.Status = schemas.StepBlocked
		s.Mu.Unlock()
		s.addDecision(graph, step, schemas.DecisionBudgetExhausted, map[string]any{
			"error":        budgetErr.Error(),
			"tokens_used":  graph.TokensUsed,
			"token_budget": graph.TokenBudget,
		}, []string{"resume", "abort"})
		return false
	}

	// Stop before dispatch when the known prompt-side cost already overruns the
	// budget. Output tokens remain covered by BudgetGuard's safeMargin.
	if graph.TokenBudget > 0 {
		projected := int64(estimateStepPromptTokens(step))
		if projected > 0 {
			s.Mu.RLock()
			lookaheadErr := s.budgetGuard.CheckBudgetLookahead(graph, projected)
			s.Mu.RUnlock()
			if lookaheadErr != nil {
				s.Mu.Lock()
				graph.BudgetPaused = true
				step.Status = schemas.StepBlocked
				s.Mu.Unlock()
				s.addDecision(graph, step, schemas.DecisionBudgetExhausted, map[string]any{
					"error":          lookaheadErr.Error(),
					"tokens_used":    graph.TokensUsed,
					"projected_cost": projected,
					"token_budget":   graph.TokenBudget,
				}, []string{"resume", "abort"})
				return false
			}
		}
	}
	return true
}

// observeBudgetSentinel snapshots graph counters under RLock, then performs
// logging and the optional abort request outside the lock (audit M4).
func (s *DirectedEngine) observeBudgetSentinel(graph *schemas.TaskGraph, step *schemas.Step) {
	if s.budgetSentinel == nil {
		return
	}
	s.Mu.RLock()
	tokensUsed := graph.TokensUsed
	tokenBudget := graph.TokenBudget
	taskID := graph.TaskID
	totalSteps, doneSteps := 0, 0
	for _, st := range graph.Steps {
		totalSteps++
		if st.Status == schemas.StepOK || st.Status == schemas.StepFailed {
			doneSteps++
		}
	}
	s.Mu.RUnlock()

	if tokenBudget > 0 && tokensUsed > 0 {
		pct := int(100 * tokensUsed / tokenBudget)
		predictedPct := 0
		if doneSteps > 0 && totalSteps > 0 {
			predictedPct = pct * totalSteps / doneSteps
		}
		if tier := s.budgetSentinel.Observe(taskID, pct, predictedPct); tier > 0 {
			s.logger.Log(obs.EventBudgetPredictExceed, taskID, step.ID, map[string]any{
				"tier":          tier,
				"pct_used":      pct,
				"predicted_pct": predictedPct,
				"tokens_used":   tokensUsed,
				"token_budget":  tokenBudget,
				"steps_done":    doneSteps,
				"steps_total":   totalSteps,
			})
			if tier >= 90 {
				s.RequestAutonomousAbort(taskID, step.ID, FailureClassCostOverrun,
					fmt.Sprintf("budget predicted to exceed: %d%% used, %d%% predicted (%d/%d tokens)", pct, predictedPct, tokensUsed, tokenBudget))
			}
		}
	}
}

func (s *DirectedEngine) beginStagedStep(graph *schemas.TaskGraph, step *schemas.Step) (*StagedWorkspace, *SnapshotIndex) {
	if !s.registry.System.StagingEnabled {
		return nil, nil
	}
	stagedWS := NewStagedWorkspace(filepath.Join(s.outputBase, graph.TaskID))
	if snap, serr := stagedWS.TakePreStepSnapshot(step.ID); serr == nil {
		return stagedWS, snap
	} else {
		s.logger.Log("EventStagingError", graph.TaskID, step.ID, map[string]any{"phase": "pre_snapshot", "error": serr.Error()})
		return stagedWS, nil
	}
}

// restoreTaskLocalClosure re-registers task-local JIT tools after global TTL GC.
func (s *DirectedEngine) restoreTaskLocalClosure(graph *schemas.TaskGraph, step *schemas.Step) {
	if s.jit != nil && len(graph.LocalMCPs) > 0 {
		if restored := s.jit.RestoreLocalMCPs(graph.LocalMCPs); len(restored) > 0 {
			s.logger.Log("JITClosureRestored", graph.TaskID, step.ID, map[string]any{
				"restored_ids": restored,
			})
		}
	}
}

// waitForStepRetry keeps cancellation responsive and stops timers on cancel.
func waitForStepRetry(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
