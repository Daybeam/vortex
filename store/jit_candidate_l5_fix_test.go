package store

import (
	"testing"
)

// TestUpsertTaskPatternFromReplay_Accumulation is the regression test for the
// L5 triple-breakage fix (docs/RSI_AUTONOMY_LEVELS_ASSESSMENT.md).
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
	es.UpsertTaskPatternFromReplay("jit_001", "RewriteStrategy", "build a widget", true)

	p, ok := es.TaskPatterns["jit_001"]
	if !ok {
		t.Fatal("expected pattern to be created in TaskPatterns")
	}
	if p.SampleCount != 1 {
		t.Errorf("expected SampleCount=1 after first write, got %d", p.SampleCount)
	}

	// Simulate 4 more replay cycles (6h each = 24h total)
	for i := 0; i < 4; i++ {
		es.UpsertTaskPatternFromReplay("jit_002", "RewriteStrategy", "build a widget", true)
	}

	// After 5 total writes, SampleCount should be 5
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

	es.UpsertTaskPatternFromReplay("jit_001", "RewriteStrategy", "rewrite", true)
	es.UpsertTaskPatternFromReplay("jit_002", "AlternativeToolchain", "alt toolchain", true)

	if len(es.TaskPatterns) != 2 {
		t.Errorf("expected 2 patterns for different SequenceKeys, got %d", len(es.TaskPatterns))
	}
}

// TestUpsertTaskPatternFromReplay_VisibleToQueryJITCandidates verifies the
// data flow closure: after the fix, replay patterns land in TaskPatterns
// (the map QueryJITCandidates reads from), not just JITCandidates.
func TestUpsertTaskPatternFromReplay_VisibleToQueryJITCandidates(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns: make(map[string]TaskPattern),
	}

	es.UpsertTaskPatternFromReplay("jit_001", "RewriteStrategy", "build a widget", true)

	if len(es.TaskPatterns) == 0 {
		t.Error("expected pattern in TaskPatterns after replay write-back, but TaskPatterns is empty (L5 Break 1 not fixed)")
	}

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

// TestUpsertTaskPatternFromReplay_VerifiedConfidenceConverges verifies
// Break 3 fix: when verified=true, AvgConfidence converges toward 0.8
// (above the 0.7 gate), NOT stuck at 0.5.
func TestUpsertTaskPatternFromReplay_VerifiedConfidenceConverges(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns: make(map[string]TaskPattern),
	}

	// Simulate 5 verified replay cycles
	for i := 0; i < 5; i++ {
		es.UpsertTaskPatternFromReplay("jit_001", "RewriteStrategy", "build a widget", true)
	}

	var found *TaskPattern
	for _, p := range es.TaskPatterns {
		if p.SequenceKey == "RewriteStrategy" {
			found = &p
			break
		}
	}
	if found == nil {
		t.Fatal("pattern not found")
	}

	// After 5 verified cycles, AvgConfidence should be 0.8 (all samples are 0.8)
	if found.AvgConfidence < 0.7 {
		t.Errorf("Break 3 NOT fixed: AvgConfidence=%v after 5 verified cycles, need >= 0.7 for gate", found.AvgConfidence)
	}
	t.Logf("AvgConfidence after 5 verified cycles = %v (gate requires >= 0.7)", found.AvgConfidence)
}

// TestUpsertTaskPatternFromReplay_UnverifiedConfidenceStaysLow verifies
// that unverified mutations (verified=false) keep AvgConfidence at 0.5,
// below the 0.7 gate — they should NOT be promoted to production.
func TestUpsertTaskPatternFromReplay_UnverifiedConfidenceStaysLow(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns: make(map[string]TaskPattern),
	}

	for i := 0; i < 5; i++ {
		es.UpsertTaskPatternFromReplay("jit_001", "RewriteStrategy", "build a widget", false)
	}

	var found *TaskPattern
	for _, p := range es.TaskPatterns {
		if p.SequenceKey == "RewriteStrategy" {
			found = &p
			break
		}
	}
	if found == nil {
		t.Fatal("pattern not found")
	}

	if found.AvgConfidence >= 0.7 {
		t.Errorf("unverified mutations should NOT reach 0.7 gate, but AvgConfidence=%v", found.AvgConfidence)
	}
}
