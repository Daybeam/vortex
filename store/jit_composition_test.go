package store

import (
	"context"
	"testing"
)

// newTestExperienceStore builds a bare ExperienceStore with its maps
// initialized directly (bypassing NewExperienceStore's disk-backed
// constructor), matching the lightweight construction style other tests in
// this package already use for pure in-memory logic tests.
func newTestExperienceStore() *ExperienceStore {
	return &ExperienceStore{
		backend:          &FileExperienceBackend{dir: "test"},
		TaskPatterns:     make(map[string]TaskPattern),
		RoleProfiles:     make(map[string]*RoleProfile),
		SkillAffinities:  make(map[string]*SkillAffinity),
		GeneratedSkills:  make(map[string]*GeneratedSkill),
		ArchivedSkills:   make(map[string]*GeneratedSkill),
		RoutingMatrix:    make(map[string]map[string]map[string]map[string]*RouteWeight),
		DecisionOutcomes: nil,
	}
}

func sampleRecords(task string) []StepRecord {
	return []StepRecord{
		{RoleID: "system_coder", Capability: "code", Skills: []string{"mcp_builder"}, Task: task},
		{RoleID: "system_coder", Capability: "verify", Skills: nil, Task: task},
	}
}

// TestUpsertTaskPattern_RepeatedSequenceAccumulatesSampleCount is the direct
// regression test for the 2026-07-28 fix: before the fix, every call to
// upsertTaskPattern created a brand-new pattern with SampleCount pinned at
// 1, regardless of how many times the exact same step sequence had run.
func TestUpsertTaskPattern_RepeatedSequenceAccumulatesSampleCount(t *testing.T) {
	es := newTestExperienceStore()

	es.upsertTaskPattern("task_1", "code_and_verify", sampleRecords("build a widget"), 0.8)
	if len(es.TaskPatterns) != 1 {
		t.Fatalf("expected 1 pattern after first call, got %d", len(es.TaskPatterns))
	}

	es.upsertTaskPattern("task_2", "code_and_verify", sampleRecords("build a different widget"), 1.0)
	if len(es.TaskPatterns) != 1 {
		t.Fatalf("expected the second call (same step sequence) to merge into the existing pattern, got %d distinct patterns", len(es.TaskPatterns))
	}

	var p TaskPattern
	for _, v := range es.TaskPatterns {
		p = v
	}
	if p.SampleCount != 2 {
		t.Fatalf("SampleCount = %d, want 2 after two matching calls", p.SampleCount)
	}
	wantAvg := (0.8 + 1.0) / 2
	if p.AvgConfidence < wantAvg-0.0001 || p.AvgConfidence > wantAvg+0.0001 {
		t.Fatalf("AvgConfidence = %f, want %f", p.AvgConfidence, wantAvg)
	}
}

// TestUpsertTaskPattern_DifferentSequenceStaysSeparate confirms a
// genuinely different step composition (different capability) does NOT get
// merged into an unrelated pattern just because it was submitted around the
// same time -- SequenceKey must actually distinguish them.
func TestUpsertTaskPattern_DifferentSequenceStaysSeparate(t *testing.T) {
	es := newTestExperienceStore()

	es.upsertTaskPattern("task_1", "code_and_verify", sampleRecords("build a widget"), 0.8)
	differentRecords := []StepRecord{
		{RoleID: "market_researcher", Capability: "research", Skills: []string{"exa"}, Task: "research the market"},
	}
	es.upsertTaskPattern("task_2", "research_task", differentRecords, 0.9)

	if len(es.TaskPatterns) != 2 {
		t.Fatalf("expected 2 distinct patterns for genuinely different step sequences, got %d", len(es.TaskPatterns))
	}
}

// TestQueryJITCandidates_RequiresBothSamplesAndConfidence confirms the
// threshold filter genuinely gates on both dimensions -- a pattern that's
// repeated enough but has low confidence should not be suggested, and vice
// versa.
func TestQueryJITCandidates_RequiresBothSamplesAndConfidence(t *testing.T) {
	es := newTestExperienceStore()

	// Repeat 5 times with high confidence -- should qualify.
	for i := 0; i < 5; i++ {
		es.upsertTaskPattern("t", "deploy_pipeline", sampleRecords("run the deploy pipeline"), 0.9)
	}
	// A different, low-confidence pattern, repeated just as often -- should NOT qualify.
	lowConfRecords := []StepRecord{
		{RoleID: "flaky_role", Capability: "flaky_cap", Skills: nil, Task: "run the deploy pipeline again"},
	}
	for i := 0; i < 5; i++ {
		es.upsertTaskPattern("t", "flaky_pipeline", lowConfRecords, 0.3)
	}

	got := es.QueryJITCandidates(context.Background(), "run the deploy pipeline", 5, 0.7, 5)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 qualifying candidate, got %d", len(got))
	}
	if got[0].AvgConfidence < 0.7 {
		t.Fatalf("returned candidate should meet the confidence threshold, got %f", got[0].AvgConfidence)
	}
}

// TestQueryJITCandidates_BelowSampleThresholdExcluded confirms a pattern
// that hasn't repeated enough times yet is not suggested, even with perfect
// confidence -- this is the core guard against suggesting JIT compilation
// for a one-off task.
func TestQueryJITCandidates_BelowSampleThresholdExcluded(t *testing.T) {
	es := newTestExperienceStore()
	es.upsertTaskPattern("t", "one_off_task", sampleRecords("do this exactly once"), 1.0)

	got := es.QueryJITCandidates(context.Background(), "do this exactly once", 5, 0.5, 5)
	if len(got) != 0 {
		t.Fatalf("expected no candidates below the sample-count threshold, got %d", len(got))
	}
}

// TestFormatJITCompositionHints_EmptyReturnsEmpty confirms the formatter is
// a true no-op when there's nothing to suggest, so callers can always
// append its result to a task string unconditionally-ish without checking
// len() themselves first (mirrors FormatSOPHints' same behavior).
func TestFormatJITCompositionHints_EmptyReturnsEmpty(t *testing.T) {
	if got := FormatJITCompositionHints(nil); got != "" {
		t.Fatalf("expected empty string for nil input, got %q", got)
	}
}

// TestFormatJITCompositionHints_ContainsHedgedLanguageAndJITCallForm
// confirms the rendered hint text (a) is explicitly non-mandatory (matching
// the hedged-suggestion design established by FormatSOPHints) and (b)
// references the real, correct orchestrator_invoke call shape for creating
// a JIT tool, not a guessed/incorrect one.
func TestFormatJITCompositionHints_ContainsHedgedLanguageAndJITCallForm(t *testing.T) {
	patterns := []TaskPattern{{ID: "pat_abc123", SampleCount: 7, AvgConfidence: 0.85, TaskType: "deploy_pipeline"}}
	got := FormatJITCompositionHints(patterns)
	if got == "" {
		t.Fatal("expected non-empty output for a non-empty pattern list")
	}
	mustContain := []string{
		"suggestion, not a requirement",
		"subsystem=\"jit\"",
		"action=\"create\"",
		"pat_abc123",
	}
	for _, want := range mustContain {
		if !containsSubstr(got, want) {
			t.Errorf("expected output to contain %q, got: %s", want, got)
		}
	}
}

func containsSubstr(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (needle == "" || indexOfSubstr(haystack, needle) >= 0)
}

func indexOfSubstr(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
