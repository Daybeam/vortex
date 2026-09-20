package pareto

import (
	"testing"
)

type mockSolution struct {
	id      string
	success float64 // maximize
	cost    float64 // minimize
}

func (m mockSolution) Objectives() ([]float64, []bool) {
	// success: maximize (false), cost: minimize (true)
	return []float64{m.success, m.cost}, []bool{false, true}
}

func TestFilterFrontier(t *testing.T) {
	candidates := []mockSolution{
		{id: "A", success: 0.9, cost: 10},  // Good success, low cost (Pareto optimal)
		{id: "B", success: 0.95, cost: 50}, // Higher success, high cost (Pareto optimal)
		{id: "C", success: 0.8, cost: 40},  // Worse success AND higher cost than A (Dominated!)
		{id: "D", success: 0.95, cost: 10}, // Best in both (Pareto optimal / dominates A & B)
	}

	front := FilterFrontier(candidates)

	// Expected front: D dominates A and B because D has higher/equal success and lower/equal cost.
	// Wait: D (0.95, 10) dominates B (0.95, 50) [same success, lower cost] and A (0.90, 10) [higher success, same cost].
	// Does D dominate everything? Yes!
	// Let's check: A is dominated by D (0.95 > 0.90, 10 <= 10). B is dominated by D (0.95 == 0.95, 10 < 50). C is dominated by A and D.
	// So only D should survive.

	if len(front) != 1 || front[0].id != "D" {
		t.Fatalf("Expected only [D] on Pareto frontier, got %v", front)
	}
}

func TestFilterFrontier_MultipleOptimal(t *testing.T) {
	candidates := []mockSolution{
		{id: "A", success: 0.9, cost: 10},  // Cheap & decent
		{id: "B", success: 0.98, cost: 80}, // Extremely accurate & expensive
		{id: "C", success: 0.95, cost: 30}, // Balanced trade-off
	}

	front := FilterFrontier(candidates)

	// A, B, and C should all be on the frontier because none strictly dominates another:
	// A: 0.9, 10 (cheapest)
	// B: 0.98, 80 (most accurate)
	// C: 0.95, 30 (balanced)
	if len(front) != 3 {
		t.Fatalf("Expected 3 candidates on Pareto frontier, got %d", len(front))
	}
}
