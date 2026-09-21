package pareto

// ParetoSolution defines the interface for any candidate solution
// evaluated across multiple objective functions.
type ParetoSolution interface {
	// Objectives returns objective values and optimization directions.
	// minimize[i] == true means lower objective value is better (e.g. cost, latency, length).
	// minimize[i] == false means higher objective value is better (e.g. success rate, confidence, F1).
	Objectives() (vals []float64, minimize []bool)
}

// FilterFrontier computes the Pareto frontier (non-dominated set) from a slice of candidates.
// Solution A dominates B iff A is as good as or better than B in all objectives,
// and strictly better in at least one objective.
func FilterFrontier[S ParetoSolution](candidates []S) []S {
	if len(candidates) <= 1 {
		return candidates
	}

	var front []S
	for i, cand := range candidates {
		dominated := false
		for j, other := range candidates {
			if i == j {
				continue
			}
			if dominates(other, cand) {
				dominated = true
				break
			}
		}
		if !dominated {
			front = append(front, cand)
		}
	}
	return front
}

// dominates returns true if 'a' strictly dominates 'b'.
func dominates(a, b ParetoSolution) bool {
	aVals, aMin := a.Objectives()
	bVals, bMin := b.Objectives()

	if len(aVals) != len(bVals) || len(aMin) != len(bMin) {
		return false
	}

	atLeastOneBetter := false
	for k := range aVals {
		av := aVals[k]
		bv := bVals[k]
		minimize := aMin[k]

		if minimize {
			// Lower is better
			if av > bv {
				return false // a is worse in this objective, cannot dominate
			}
			if av < bv {
				atLeastOneBetter = true
			}
		} else {
			// Higher is better
			if av < bv {
				return false // a is worse in this objective, cannot dominate
			}
			if av > bv {
				atLeastOneBetter = true
			}
		}
	}

	return atLeastOneBetter
}
