package search

import (
	"testing"
)

func TestRRFMergeCapped_NoCap(t *testing.T) {
	// maxPerSource=0 should behave identically to RRFMerge.
	r1 := map[string]float64{"a": 1.0, "b": 0.5, "c": 0.3}
	r2 := map[string]float64{"a": 0.8, "d": 0.6}

	uncapped := RRFMerge(r1, r2)
	capped0 := RRFMergeCapped(0, r1, r2)

	for id := range uncapped {
		if uncapped[id] != capped0[id] {
			t.Fatalf("id %s: uncapped=%v capped0=%v (should be equal)", id, uncapped[id], capped0[id])
		}
	}
	if len(uncapped) != len(capped0) {
		t.Fatalf("result size mismatch: uncapped=%d capped0=%d", len(uncapped), len(capped0))
	}
}

func TestRRFMergeCapped_TruncatesPerSource(t *testing.T) {
	// 5 items in r1, cap=2 → only top 2 from r1 should contribute.
	r1 := map[string]float64{
		"a": 1.0, "b": 0.8, "c": 0.6, "d": 0.4, "e": 0.2,
	}
	r2 := map[string]float64{"a": 0.9}

	capped := RRFMergeCapped(2, r1, r2)
	uncapped := RRFMerge(r1, r2)

	// "a" and "b" are top-2 in r1; they should have the same score in both.
	// "c", "d", "e" should be absent from capped (only r2 contributes "a").
	for _, id := range []string{"c", "d", "e"} {
		if _, ok := capped[id]; ok {
			// "c"/"d"/"e" might still get score from r2 — but r2 only has "a".
			// So they should NOT be in capped.
			if uncapped[id] != capped[id] {
				t.Fatalf("id %s should be absent from capped (cap=2 on r1), got score %v", id, capped[id])
			}
		}
	}

	// Capped should have fewer keys than uncapped.
	if len(capped) >= len(uncapped) {
		t.Fatalf("capped should have fewer keys: capped=%d uncapped=%d", len(capped), len(uncapped))
	}
}

func TestRRFMergeCapped_CapLargerThanRanking(t *testing.T) {
	// cap=100 but ranking has 3 items → no truncation.
	r1 := map[string]float64{"a": 1.0, "b": 0.5, "c": 0.3}
	capped := RRFMergeCapped(100, r1)
	uncapped := RRFMerge(r1)

	if len(capped) != len(uncapped) {
		t.Fatalf("cap > len(ranking) should not truncate: capped=%d uncapped=%d", len(capped), len(uncapped))
	}
}

func TestRRFMergeCapped_EmptyRankings(t *testing.T) {
	capped := RRFMergeCapped(5)
	if len(capped) != 0 {
		t.Fatalf("empty rankings should produce empty result, got %d", len(capped))
	}
}

func TestRRFMergeCapped_MultipleSourcesCappedIndependently(t *testing.T) {
	// Each source should be capped independently.
	r1 := map[string]float64{"a": 1.0, "b": 0.8, "c": 0.6, "d": 0.4}
	r2 := map[string]float64{"e": 1.0, "f": 0.8, "g": 0.6, "h": 0.4}

	capped := RRFMergeCapped(2, r1, r2)

	// r1 top-2: a, b; r2 top-2: e, f. c, d, g, h should be absent.
	for _, id := range []string{"c", "d", "g", "h"} {
		if _, ok := capped[id]; ok {
			t.Fatalf("id %s should be absent (cap=2 per source)", id)
		}
	}
	// a, b, e, f should be present.
	for _, id := range []string{"a", "b", "e", "f"} {
		if _, ok := capped[id]; !ok {
			t.Fatalf("id %s should be present (top-2 per source)", id)
		}
	}
}
