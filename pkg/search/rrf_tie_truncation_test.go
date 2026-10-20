// Regression test for RRF truncation at tie group boundaries.
//
// Bug: RRFMergeCapped's items[:maxPerSource] truncation could cut in the
// middle of a tie group. Items before the cut received RRF credit; items
// after the cut received nothing — even though they were indistinguishable
// from the ones that were kept. The tie-break key (id ascending) silently
// decided which tied items survived.
//
// Fix: extend the cap to include the entire tie group at the boundary.

package search

import (
	"math"
	"testing"
)

// TestRRFMergeCapped_TruncationAtTieBoundary verifies that when maxPerSource
// falls in the middle of a tie group, all tied items receive the same RRF
// credit — the cap does not split the group.
//
// Before the fix: with 10 flat-scored items and maxPerSource=5, items[:5]
// kept the first 5 (alphabetically) and gave them RRF credit. The remaining
// 5 got nothing, even though they were indistinguishable.
//
// After the fix: the cap extends to include all 10 tied items, so all
// receive the same credit.
func TestRRFMergeCapped_TruncationAtTieBoundary(t *testing.T) {
	// 10 items, all with the same score — a flat channel.
	flat := map[string]float64{
		"a": 1, "b": 1, "c": 1, "d": 1, "e": 1,
		"f": 1, "g": 1, "h": 1, "i": 1, "j": 1,
	}

	// Cap at 5 — right in the middle of the tie group.
	merged := RRFMergeCapped(5, flat)

	// All 10 items must be present in the merged output.
	// Before the fix: only a-e were present; f-j were truncated.
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		if _, ok := merged[id]; !ok {
			t.Errorf("item %q was truncated at tie boundary — all tied items must be included", id)
		}
	}

	// All items must receive the same score — no arbitrary split.
	first := merged["a"]
	for _, id := range []string{"b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		if math.Abs(merged[id]-first) > floatTol {
			t.Errorf("item %q scored %.15f, want %.15f — tie group was split by truncation",
				id, merged[id], first)
		}
	}
}

// TestRRFMergeCapped_TruncationRespectsNonTiedBoundary verifies that the
// tie-boundary extension only applies when the boundary falls inside a tie
// group. If items at the boundary have different scores, the cap works
// normally.
func TestRRFMergeCapped_TruncationRespectsNonTiedBoundary(t *testing.T) {
	// 5 items with distinct scores, cap at 3.
	ranking := map[string]float64{
		"a": 5, "b": 4, "c": 3, "d": 2, "e": 1,
	}

	merged := RRFMergeCapped(3, ranking)

	// Only top 3 should be present.
	for _, id := range []string{"a", "b", "c"} {
		if _, ok := merged[id]; !ok {
			t.Errorf("item %q should be in top-3", id)
		}
	}
	for _, id := range []string{"d", "e"} {
		if _, ok := merged[id]; ok {
			t.Errorf("item %q should have been truncated (score distinct from boundary)", id)
		}
	}
}
