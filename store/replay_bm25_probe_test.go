package store

import (
	"context"
	"testing"
)

// TestReplayPattern_BM25RetrievalGate empirically verifies whether a pattern
// written by UpsertTaskPatternFromReplay is actually retrievable through the
// production retrieval path (QueryJITCandidates -> QuerySimilarPatterns),
// as opposed to merely present in the TaskPatterns map.
//
// This test was written to expose the L5 Break 3 (AvgConfidence stuck at 0.5)
// and Break 4 (BM25 SourceText mismatch) issues. After the fix:
//   - verified=true → AvgConfidence converges to 0.8 > 0.7 gate (Break 3 fixed)
//   - sourceText="build a widget" → BM25 matches real task queries (Break 4 fixed)
func TestReplayPattern_BM25RetrievalGate(t *testing.T) {
	es := &ExperienceStore{TaskPatterns: make(map[string]TaskPattern)}

	// Simulate 5 verified replay cycles with real task text as SourceText
	for i := 0; i < 5; i++ {
		es.UpsertTaskPatternFromReplay("jit_"+string(rune('a'+i)), "RewriteStrategy", "build a widget", true)
	}

	// Confirm the thresholds are reached
	var found *TaskPattern
	for _, p := range es.TaskPatterns {
		if p.SequenceKey == "RewriteStrategy" {
			found = &p
			break
		}
	}
	if found == nil {
		t.Fatal("pattern with SequenceKey=RewriteStrategy not found")
	}
	t.Logf("SampleCount after 5 cycles = %d (threshold minSamples=5)", found.SampleCount)
	t.Logf("AvgConfidence = %v (gate requires >= 0.7)", found.AvgConfidence)
	t.Logf("SourceText=%q", found.SourceText)

	if found.SampleCount < 5 {
		t.Fatalf("accumulation FAILED: SampleCount=%d, want >=5", found.SampleCount)
	}
	if found.AvgConfidence < 0.7 {
		t.Fatalf("Break 3 NOT fixed: AvgConfidence=%v, need >= 0.7 for gate", found.AvgConfidence)
	}
	t.Log("PASS: SampleCount >= 5 and AvgConfidence >= 0.7")

	// Now verify BM25 retrieval with real task text
	results := es.QueryJITCandidates(context.Background(), "build a widget", 5, 0.7, 3)
	t.Logf("QueryJITCandidates(\"build a widget\") returned %d results", len(results))

	if len(results) == 0 {
		t.Error("Break 4 NOT fixed: real task text does NOT retrieve the replay pattern (BM25 gate blocks it)")
	} else {
		t.Log("PASS: real task text retrieves the replay pattern")
	}
}
