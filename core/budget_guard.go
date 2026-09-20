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

// DeductBudget increments the used tokens and checks if it exceeded the budget.
func (g *BudgetGuard) DeductBudget(graph *schemas.TaskGraph, tokens int64) bool {
	graph.TokensUsed += tokens
	if graph.TokenBudget > 0 && graph.TokensUsed >= graph.TokenBudget-g.safeMargin {
		graph.BudgetPaused = true
		return true
	}
	return false
}
