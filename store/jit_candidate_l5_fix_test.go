package store

import (
	"testing"
)

// TestUpsertTaskPatternFromReplay_Accumulation is the regression test for the
// L5 double-breakage fix (docs/RSI_AUTONOMY_LEVELS_ASSESSMENT.md).
//
// Before the fix: replay_scheduler.go:569 called AddJITCandidate which writes
// to JITCandidates map, but QueryJITCandidates reads from TaskPatterns map.
// Two different maps — the write side never touched TaskPatterns, so replay
// candidates were never visible to the retrieval path.
//
// After the fix: UpsertTaskPatternFromReplay writes to TaskPatterns with
// accumulation semantics (SampleCount increments on repeated calls with the
// same SequenceKey), so after enough replay cycles the pattern reaches the
// minSamples threshold and becomes visible to QueryJITCandidates.
func TestUpsertTaskPatternFromReplay_Accumulation(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns: make(map[string]TaskPattern),
	}

	// First replay cycle — creates new pattern with SampleCount=1
	es.UpsertTaskPatternFromReplay("jit_001", "RewriteStrategy", "rewrite mutation from replay")

	p, ok := es.TaskPatterns["jit_001"]
	if !ok {
		t.Fatal("expected pattern to be created in TaskPatterns")
	}
	if p.SampleCount != 1 {
		t.Errorf("expected SampleCount=1 after first write, got %d", p.SampleCount)
	}

	// Simulate 4 more replay cycles (6h each = 24h total)
	// Each cycle should increment SampleCount via accumulation
	for i := 0; i < 4; i++ {
		es.UpsertTaskPatternFromReplay("jit_002", "RewriteStrategy", "rewrite mutation from replay")
	}

	// After 5 total writes (1 + 4), the pattern should have SampleCount=5
	// Find the pattern by SequenceKey
	var found *TaskPattern
	for _, p := range es.TaskPatterns {
		if p.SequenceKey == "RewriteStrategy" {
			found = &p
			break
		}
	}
	if found == nil {
		t.Fatal("expected to find pattern with SequenceKey=RewriteStrategy")
	}
	if found.SampleCount != 5 {
		t.Errorf("expected SampleCount=5 after 5 replay cycles, got %d", found.SampleCount)
	}
}

// TestUpsertTaskPatternFromReplay_DifferentSequenceKeys verifies that
// patterns with different SequenceKeys create separate entries.
func TestUpsertTaskPatternFromReplay_DifferentSequenceKeys(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns: make(map[string]TaskPattern),
	}

	es.UpsertTaskPatternFromReplay("jit_001", "RewriteStrategy", "rewrite")
	es.UpsertTaskPatternFromReplay("jit_002", "AlternativeToolchain", "alt toolchain")

	if len(es.TaskPatterns) != 2 {
		t.Errorf("expected 2 patterns for different SequenceKeys, got %d", len(es.TaskPatterns))
	}
}

// TestUpsertTaskPatternFromReplay_VisibleToQueryJITCandidates verifies the
// data flow closure: after the fix, replay patterns land in TaskPatterns
// (the map QueryJITCandidates reads from), not just JITCandidates (the map
// that was never read). Before the fix, TaskPatterns was empty after replay.
func TestUpsertTaskPatternFromReplay_VisibleToQueryJITCandidates(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns: make(map[string]TaskPattern),
	}

	// Before fix: AddJITCandidate writes to JITCandidates, TaskPatterns stays empty.
	// After fix: UpsertTaskPatternFromReplay writes to TaskPatterns.
	es.UpsertTaskPatternFromReplay("jit_001", "RewriteStrategy", "rewrite mutation from replay")

	if len(es.TaskPatterns) == 0 {
		t.Error("expected pattern in TaskPatterns after replay write-back, but TaskPatterns is empty (L5 Break 1 not fixed)")
	}

	// Verify the pattern has the expected fields
	p, ok := es.TaskPatterns["jit_001"]
	if !ok {
		t.Fatal("expected pattern jit_001 in TaskPatterns")
	}
	if p.SequenceKey != "RewriteStrategy" {
		t.Errorf("expected SequenceKey=RewriteStrategy, got %q", p.SequenceKey)
	}
	if p.SampleCount != 1 {
		t.Errorf("expected SampleCount=1, got %d", p.SampleCount)
	}
}
