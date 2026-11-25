package core

import (
	"fmt"

	"github.com/daybeam/vortex/schemas"
)

// BudgetGuard manages token usage tracking and enforcement for task graphs.
type BudgetGuard struct {
	safeMargin int64
}

func NewBudgetGuard(safeMargin int64) *BudgetGuard {
	if safeMargin <= 0 {
		safeMargin = 2000 // Default safety margin
	}
	return &BudgetGuard{safeMargin: safeMargin}
}

// CheckBudget returns an error if the task has exceeded its budget.
func (g *BudgetGuard) CheckBudget(graph *schemas.TaskGraph) error {
	if graph.TokenBudget > 0 && graph.TokensUsed >= graph.TokenBudget-g.safeMargin {
		return fmt.Errorf("token budget exceeded: used %d/%d", graph.TokensUsed, graph.TokenBudget)
	}
	return nil
}

// CheckBudgetLookahead returns an error if the task's used tokens PLUS a
// projected cost for the next step would exceed the budget. This is the
// OCR (alibaba/open-code-review scan/agent.go dispatchBatch) pattern: stop
// BEFORE dispatching work that is already known to overrun, instead of
// discovering the overrun after the step has spent its tokens.
//
// projected is the prompt-side estimate for the step about to run
// (providers.EstimateTokens on its task text plus role template overhead).
// Output tokens cannot be estimated cheaply, so the existing safeMargin
// covers that side; the look-ahead only refuses work whose input alone
// already blows the budget. projected <= 0 disables the look-ahead
// (unknown cost ⇒ fall back to the plain CheckBudget semantics).
// Like CheckBudget, it only applies when TokenBudget > 0.
func (g *BudgetGuard) CheckBudgetLookahead(graph *schemas.TaskGraph, projected int64) error {
	if projected <= 0 || graph.TokenBudget <= 0 {
		return nil
	}
	if graph.TokensUsed+projected > graph.TokenBudget-g.safeMargin {
		return fmt.Errorf("token budget would be exceeded: used %d + projected %d > budget %d",
			graph.TokensUsed, projected, graph.TokenBudget)
	}
	return nil
}

// DeductBudget increments the used tokens and checks if it exceeded the budget.
func (g *BudgetGuard) DeductBudget(graph *schemas.TaskGraph, tokens int64) bool {
	graph.TokensUsed += tokens
	if graph.TokenBudget > 0 && graph.TokensUsed >= graph.TokenBudget-g.safeMargin {
		graph.BudgetPaused = true
		return true
	}
	return false
}
