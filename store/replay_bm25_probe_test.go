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
// Context: the existing test TestUpsertTaskPatternFromReplay_VisibleToQueryJITCandidates
// is named as if it verifies retrieval-path closure, but it only asserts that
// es.TaskPatterns is non-empty and never calls QueryJITCandidates. This test
// closes that gap.
func TestReplayPattern_BM25RetrievalGate(t *testing.T) {
	es := &ExperienceStore{TaskPatterns: make(map[string]TaskPattern)}

	// Simulate 5 replay cycles (reaches minSamples=5 threshold)
	for i := 0; i < 5; i++ {
		es.UpsertTaskPatternFromReplay("jit_"+string(rune('a'+i)), "RewriteStrategy", "rewrite mutation from replay")
	}

	// Confirm the threshold IS reached (accumulation works)
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
	t.Logf("SourceText=%q TaskType=%q", found.SourceText, found.TaskType)

	if found.SampleCount < 5 {
		t.Fatalf("accumulation FAILED: SampleCount=%d, want >=5", found.SampleCount)
	}
	t.Log("PASS: accumulation threshold reached")

	// Now the real question: is it retrievable via the production path?
	// tools_task.go:348 calls QueryJITCandidates(ctx, inputs[0].Task, 5, 0.7, 3)
	// where inputs[0].Task is the REAL task text, e.g. "build a widget".
	results := es.QueryJITCandidates(context.Background(), "build a widget", 5, 0.7, 3)
	t.Logf("QueryJITCandidates(\"build a widget\") returned %d results", len(results))

	// Even a looser query matching the SourceText verbatim:
	results2 := es.QueryJITCandidates(context.Background(), "rewrite mutation from replay", 5, 0.7, 3)
	t.Logf("QueryJITCandidates(\"rewrite mutation from replay\") returned %d results", len(results2))

	// Lower the confidence gate to isolate whether BM25 alone blocks retrieval
	results3 := es.QueryJITCandidates(context.Background(), "rewrite mutation from replay", 5, 0.0, 3)
	t.Logf("QueryJITCandidates(\"rewrite mutation from replay\", conf=0.0) returned %d results", len(results3))

	// Isolate the confidence gate: conf=0.0 lets BM25 through, so this tells us
	// whether AvgConfidence can ever reach the 0.7 gate. If AvgConfidence is
	// permanently 0.5 (see UpsertTaskPatternFromReplay: it always passes 0.5 as
	// the new sample, so the running average never moves), the gate at 0.7 is
	// unreachable regardless of cycle count.
	results4 := es.QueryJITCandidates(context.Background(), "rewrite mutation from replay", 5, 0.49, 3)
	t.Logf("QueryJITCandidates(\"rewrite mutation from replay\", conf=0.49) returned %d results", len(results4))
	results5 := es.QueryJITCandidates(context.Background(), "rewrite mutation from replay", 5, 0.51, 3)
	t.Logf("QueryJITCandidates(\"rewrite mutation from replay\", conf=0.51) returned %d results", len(results5))
	t.Logf("AvgConfidence after 5 cycles = %v (gate requires >= 0.7)", found.AvgConfidence)

	if len(results) > 0 {
		t.Log("OK: real task text retrieves the replay pattern")
	} else {
		t.Log("NOTE: real task text does NOT retrieve the replay pattern (BM25 gate blocks it)")
	}
}
