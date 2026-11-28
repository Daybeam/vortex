package core

// RefinementVerdict is the 4-case outcome of comparing two review scores
// in a render-then-review refinement loop (borrowed from PaperOrchestra's
// ContentRefinementAgent). The gate is authoritative: a revert verdict
// MUST NOT advance the delivered artifact pointer.
type RefinementVerdict int

const (
	// VerdictAccept: overall improved, or flat with gain-sum >= drop-sum.
	VerdictAccept RefinementVerdict = iota
	// VerdictRevertRegression: overall score decreased.
	VerdictRevertRegression
	// VerdictRevertSubAxisDrift: overall flat but drop-sum > gain-sum.
	VerdictRevertSubAxisDrift
	// VerdictKeepPreviousRenderFail: candidate failed to render.
	VerdictKeepPreviousRenderFail
)

// ReviewScores holds the structured rubric scores from one review round.
// Overall is the composite score (required); Axes are named per-axis scores
// (e.g. clarity, layout, fidelity). Overall is compared separately from
// the sub-axis gain/drop calculation.
type ReviewScores struct {
	Overall float64
	Axes    map[string]float64
}

// GateRefinement decides accept/revert for one refinement round by comparing
// previous and next review scores. Returns the verdict plus the total gain
// and drop across sub-axes (excluding Overall, which is compared separately).
//
// 4-case logic (maps 1:1 from PaperOrchestra run() Cases 1/2/3a/3b):
//   - renderOK=false → VerdictKeepPreviousRenderFail
//   - next.Overall > prev.Overall → VerdictAccept
//   - next.Overall < prev.Overall → VerdictRevertRegression
//   - next.Overall == prev.Overall → accept iff gain-sum >= drop-sum
//
// gain/drop are computed over axes present in BOTH prev and next.
// Axes only in one side are ignored (no half-comparisons).
func GateRefinement(prev, next ReviewScores, renderOK bool) (RefinementVerdict, float64, float64) {
	if !renderOK {
		return VerdictKeepPreviousRenderFail, 0, 0
	}

	// Case 1: overall improved.
	if next.Overall > prev.Overall {
		gain, drop := axisDeltas(prev.Axes, next.Axes)
		return VerdictAccept, gain, drop
	}

	// Case 2: overall regressed.
	if next.Overall < prev.Overall {
		gain, drop := axisDeltas(prev.Axes, next.Axes)
		return VerdictRevertRegression, gain, drop
	}

	// Case 3: overall flat — decide by sub-axis gain/drop.
	gain, drop := axisDeltas(prev.Axes, next.Axes)
	if gain >= drop {
		return VerdictAccept, gain, drop
	}
	return VerdictRevertSubAxisDrift, gain, drop
}

// axisDeltas computes the total gain and drop across axes present in both
// prev and next. gain = sum of positive deltas; drop = sum of |negative deltas|.
// Axes only in one map are ignored.
func axisDeltas(prev, next map[string]float64) (gain, drop float64) {
	for k, pv := range prev {
		nv, ok := next[k]
		if !ok {
			continue
		}
		d := nv - pv
		if d > 0 {
			gain += d
		} else if d < 0 {
			drop += -d
		}
	}
	return gain, drop
}

// VerdictString returns a human-readable label for the verdict.
func (v RefinementVerdict) String() string {
	switch v {
	case VerdictAccept:
		return "accepted"
	case VerdictRevertRegression:
		return "reverted_regression"
	case VerdictRevertSubAxisDrift:
		return "reverted_subaxis_drift"
	case VerdictKeepPreviousRenderFail:
		return "kept_previous_render_fail"
	default:
		return "unknown"
	}
}
