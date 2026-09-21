package core

import (
	"testing"

	"github.com/daybeam/vortex/schemas"
)

func TestBudgetGuard(t *testing.T) {
	bg := NewBudgetGuard(100) // 100 token safety margin

	graph := &schemas.TaskGraph{
		TokenBudget: 1000,
		TokensUsed:  800,
	}

	// Should pass
	if err := bg.CheckBudget(graph); err != nil {
		t.Errorf("CheckBudget failed: %v", err)
	}

	// Deduct tokens (should pause because 800+150=950 >= 1000-100)
	paused := bg.DeductBudget(graph, 150)
	if !paused {
		t.Errorf("DeductBudget should have paused (hit safety margin)")
	}
	if graph.TokensUsed != 950 {
		t.Errorf("Expected 950 tokens used, got %d", graph.TokensUsed)
	}
	if !graph.BudgetPaused {
		t.Error("graph.BudgetPaused should be true")
	}

	// Should fail (within safety margin)
	if err := bg.CheckBudget(graph); err == nil {
		t.Error("CheckBudget should have failed within safety margin")
	}
}
