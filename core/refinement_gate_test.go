package core

import (
	"math"
	"testing"
)

func TestGateRefinement_RenderFail(t *testing.T) {
	prev := ReviewScores{Overall: 5.0, Axes: map[string]float64{"layout": 5}}
	next := ReviewScores{Overall: 9.0, Axes: map[string]float64{"layout": 9}}
	v, gain, drop := GateRefinement(prev, next, false)
	if v != VerdictKeepPreviousRenderFail {
		t.Errorf("render fail: got %v, want VerdictKeepPreviousRenderFail", v)
	}
	if gain != 0 || drop != 0 {
		t.Errorf("render fail: gain=%v drop=%v, want 0/0", gain, drop)
	}
}

func TestGateRefinement_OverallIncrease(t *testing.T) {
	prev := ReviewScores{Overall: 5.0, Axes: map[string]float64{"layout": 5, "clarity": 5}}
	next := ReviewScores{Overall: 7.0, Axes: map[string]float64{"layout": 7, "clarity": 5}}
	v, gain, drop := GateRefinement(prev, next, true)
	if v != VerdictAccept {
		t.Errorf("overall up: got %v, want VerdictAccept", v)
	}
	if gain != 2 || drop != 0 {
		t.Errorf("overall up: gain=%v drop=%v, want 2/0", gain, drop)
	}
}

func TestGateRefinement_OverallDecrease(t *testing.T) {
	prev := ReviewScores{Overall: 7.0, Axes: map[string]float64{"layout": 7, "clarity": 7}}
	next := ReviewScores{Overall: 5.0, Axes: map[string]float64{"layout": 5, "clarity": 7}}
	v, gain, drop := GateRefinement(prev, next, true)
	if v != VerdictRevertRegression {
		t.Errorf("overall down: got %v, want VerdictRevertRegression", v)
	}
	if gain != 0 || drop != 2 {
		t.Errorf("overall down: gain=%v drop=%v, want 0/2", gain, drop)
	}
}

func TestGateRefinement_FlatGainGeDrop(t *testing.T) {
	prev := ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 5, "clarity": 7}}
	next := ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 7, "clarity": 6}}
	v, gain, drop := GateRefinement(prev, next, true)
	if v != VerdictAccept {
		t.Errorf("flat gain>=drop: got %v, want VerdictAccept", v)
	}
	// layout +2, clarity -1 → gain=2, drop=1
	if gain != 2 || drop != 1 {
		t.Errorf("flat gain>=drop: gain=%v drop=%v, want 2/1", gain, drop)
	}
}

func TestGateRefinement_FlatDropGtGain(t *testing.T) {
	prev := ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 5, "clarity": 7}}
	next := ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 6, "clarity": 4}}
	v, gain, drop := GateRefinement(prev, next, true)
	if v != VerdictRevertSubAxisDrift {
		t.Errorf("flat drop>gain: got %v, want VerdictRevertSubAxisDrift", v)
	}
	// layout +1, clarity -3 → gain=1, drop=3
	if gain != 1 || drop != 3 {
		t.Errorf("flat drop>gain: gain=%v drop=%v, want 1/3", gain, drop)
	}
}

func TestGateRefinement_FlatExactEqual(t *testing.T) {
	prev := ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 5, "clarity": 5}}
	next := ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 7, "clarity": 3}}
	v, gain, drop := GateRefinement(prev, next, true)
	// gain=2, drop=2 → gain >= drop → accept
	if v != VerdictAccept {
		t.Errorf("flat exact equal: got %v, want VerdictAccept (gain>=drop)", v)
	}
	if gain != 2 || drop != 2 {
		t.Errorf("flat exact equal: gain=%v drop=%v, want 2/2", gain, drop)
	}
}

func TestGateRefinement_AxesOnlyInOneSide(t *testing.T) {
	prev := ReviewScores{Overall: 5.0, Axes: map[string]float64{"layout": 5, "old_axis": 3}}
	next := ReviewScores{Overall: 7.0, Axes: map[string]float64{"layout": 7, "new_axis": 9}}
	v, gain, drop := GateRefinement(prev, next, true)
	if v != VerdictAccept {
		t.Errorf("axes only in one side: got %v, want VerdictAccept (overall up)", v)
	}
	// Only "layout" is in both: +2 gain, 0 drop. old_axis and new_axis ignored.
	if gain != 2 || drop != 0 {
		t.Errorf("axes only in one side: gain=%v drop=%v, want 2/0", gain, drop)
	}
}

func TestGateRefinement_EmptyAxes(t *testing.T) {
	prev := ReviewScores{Overall: 5.0}
	next := ReviewScores{Overall: 7.0}
	v, gain, drop := GateRefinement(prev, next, true)
	if v != VerdictAccept {
		t.Errorf("empty axes: got %v, want VerdictAccept", v)
	}
	if gain != 0 || drop != 0 {
		t.Errorf("empty axes: gain=%v drop=%v, want 0/0", gain, drop)
	}
}

func TestGateRefinement_FlatAllAxesUnchanged(t *testing.T) {
	prev := ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 5, "clarity": 5}}
	next := ReviewScores{Overall: 6.0, Axes: map[string]float64{"layout": 5, "clarity": 5}}
	v, gain, drop := GateRefinement(prev, next, true)
	// gain=0, drop=0 → gain >= drop → accept
	if v != VerdictAccept {
		t.Errorf("flat all unchanged: got %v, want VerdictAccept (gain=0>=drop=0)", v)
	}
	if gain != 0 || drop != 0 {
		t.Errorf("flat all unchanged: gain=%v drop=%v, want 0/0", gain, drop)
	}
}

func TestGateRefinement_FloatingPointPrecision(t *testing.T) {
	prev := ReviewScores{Overall: 5.0, Axes: map[string]float64{"layout": 5.0}}
	next := ReviewScores{Overall: 5.0, Axes: map[string]float64{"layout": 5.0 + 1e-10}}
	v, _, _ := GateRefinement(prev, next, true)
	// Tiny positive delta → gain >= drop → accept
	if v != VerdictAccept {
		t.Errorf("floating point: got %v, want VerdictAccept", v)
	}
}

func TestVerdictString(t *testing.T) {
	cases := []struct {
		v    RefinementVerdict
		want string
	}{
		{VerdictAccept, "accepted"},
		{VerdictRevertRegression, "reverted_regression"},
		{VerdictRevertSubAxisDrift, "reverted_subaxis_drift"},
		{VerdictKeepPreviousRenderFail, "kept_previous_render_fail"},
	}
	for _, c := range cases {
		if c.v.String() != c.want {
			t.Errorf("VerdictString(%d): got %q, want %q", c.v, c.v.String(), c.want)
		}
	}
}

// TestGateRefinement_TruthTable is the full 4-case truth table from the
// PaperOrchestra run() mapping (RENDER_THEN_REVIEW_REFINEMENT_LOOP.md §2.2).
func TestGateRefinement_TruthTable(t *testing.T) {
	// Case 1: overall ↑ → accept
	v, _, _ := GateRefinement(
		ReviewScores{Overall: 5.0, Axes: map[string]float64{"a": 5}},
		ReviewScores{Overall: 6.0, Axes: map[string]float64{"a": 6}},
		true,
	)
	if v != VerdictAccept {
		t.Errorf("Case 1 (overall up): got %v", v)
	}

	// Case 2: overall ↓ → revert regression
	v, _, _ = GateRefinement(
		ReviewScores{Overall: 6.0, Axes: map[string]float64{"a": 6}},
		ReviewScores{Overall: 5.0, Axes: map[string]float64{"a": 5}},
		true,
	)
	if v != VerdictRevertRegression {
		t.Errorf("Case 2 (overall down): got %v", v)
	}

	// Case 3a: overall =, gain >= drop → accept
	v, _, _ = GateRefinement(
		ReviewScores{Overall: 6.0, Axes: map[string]float64{"a": 4, "b": 8}},
		ReviewScores{Overall: 6.0, Axes: map[string]float64{"a": 6, "b": 7}},
		true,
	)
	if v != VerdictAccept {
		t.Errorf("Case 3a (flat, gain>=drop): got %v", v)
	}

	// Case 3b: overall =, drop > gain → revert sub-axis drift
	v, _, _ = GateRefinement(
		ReviewScores{Overall: 6.0, Axes: map[string]float64{"a": 4, "b": 8}},
		ReviewScores{Overall: 6.0, Axes: map[string]float64{"a": 5, "b": 5}},
		true,
	)
	if v != VerdictRevertSubAxisDrift {
		t.Errorf("Case 3b (flat, drop>gain): got %v", v)
	}

	// Case 4: render fail → keep previous
	v, _, _ = GateRefinement(
		ReviewScores{Overall: 5.0, Axes: map[string]float64{"a": 5}},
		ReviewScores{Overall: 9.0, Axes: map[string]float64{"a": 9}},
		false,
	)
	if v != VerdictKeepPreviousRenderFail {
		t.Errorf("Case 4 (render fail): got %v", v)
	}
}

// Ensure no NaN/Inf issues with edge scores.
func TestGateRefinement_ZeroScores(t *testing.T) {
	prev := ReviewScores{Overall: 0, Axes: map[string]float64{"a": 0}}
	next := ReviewScores{Overall: 0, Axes: map[string]float64{"a": 0}}
	v, gain, drop := GateRefinement(prev, next, true)
	if v != VerdictAccept {
		t.Errorf("zero scores: got %v, want VerdictAccept (gain=0>=drop=0)", v)
	}
	if math.IsNaN(gain) || math.IsNaN(drop) {
		t.Errorf("zero scores: NaN in gain/drop")
	}
}
