// Invariant: in RRF, items that a channel scores identically must receive the
// same rank credit. Rank-based fusion cannot extract an ordering from a channel
// that never expressed one, so handing tied items sequential ranks fabricates
// information -- and whatever the tie-break key happens to be (id ascending)
// ends up deciding the fused result.

package search

import (
	"math"
	"testing"
)

const floatTol = 1e-12

// TestRRFMergeCapped_TiedItemsShareRankCredit builds the minimal case where the
// two channels disagree about ordering but one of them has no ordering at all:
//
//	flat: every member of the family scores 1 -> no information
//	real: fam_c > fam_b > fam_a
//
// The fused result must follow `real`. Before tie-aware ranking, `flat`
// contributed 1/60, 1/61, 1/62 in tie-break order, which is exactly the reverse
// of `real`'s contribution here, so fam_a and fam_c came out numerically equal
// and the tie-break key silently decided the winner.
func TestRRFMergeCapped_TiedItemsShareRankCredit(t *testing.T) {
	flat := map[string]float64{"fam_a": 1, "fam_b": 1, "fam_c": 1}
	real := map[string]float64{"fam_c": 9, "fam_b": 5, "fam_a": 1}

	merged := RRFMerge(flat, real)

	if merged["fam_c"] <= merged["fam_b"] || merged["fam_b"] <= merged["fam_a"] {
		t.Fatalf("fused order must follow the discriminating channel, got a=%.6f b=%.6f c=%.6f",
			merged["fam_a"], merged["fam_b"], merged["fam_c"])
	}
}

// TestRRFMergeCapped_FlatChannelIsAdditiveConstant checks the mechanism
// directly: a fully flat channel must shift every member by the same amount, so
// it cannot reorder anything on its own.
func TestRRFMergeCapped_FlatChannelIsAdditiveConstant(t *testing.T) {
	flat := map[string]float64{"fam_a": 1, "fam_b": 1, "fam_c": 1}
	real := map[string]float64{"fam_c": 3, "fam_b": 2, "fam_a": 1}

	withFlat := RRFMerge(flat, real)
	withoutFlat := RRFMerge(real)

	for _, id := range []string{"fam_a", "fam_b", "fam_c"} {
		delta := withFlat[id] - withoutFlat[id]
		// Every member shares rank (0+1+2)/2 = 1 -> identical contribution.
		want := 1.0 / (60.0 + 1.0)
		if math.Abs(delta-want) > floatTol {
			t.Errorf("%s: flat channel contributed %.15f, want %.15f (all members must share one credit)",
				id, delta, want)
		}
	}
}

// TestRRFMergeCapped_AllChannelsFlatProducesTies verifies the degenerate case:
// when no channel carries ordering information, the output must be a tie rather
// than an ordering implied by the tie-break key.
func TestRRFMergeCapped_AllChannelsFlatProducesTies(t *testing.T) {
	flat := map[string]float64{"fam_a": 1, "fam_b": 1, "fam_c": 1}

	merged := RRFMerge(flat)

	want := merged["fam_a"]
	for _, id := range []string{"fam_b", "fam_c"} {
		if math.Abs(merged[id]-want) > floatTol {
			t.Errorf("%s scored %.15f, want %.15f (an uninformative channel must not order items)",
				id, merged[id], want)
		}
	}
}
