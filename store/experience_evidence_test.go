package store

import (
	"testing"
)

// Tests for P1 automatic evidence collection: VerifiedSampleCount accumulation
// in upsertTaskPattern and rebuildFamilyKeyStatsLocked.
// Spec: docs/completed/2026-11-27/MEMORY_TWO_TIER_KEY.md §5

func TestUpsertTaskPattern_VerifiedSampleCount_AllVerified(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok", Verified: true, Evidence: "trace_recorded",
			Trace: []ToolInteraction{{ToolName: "read_file"}}},
	}

	es.upsertTaskPattern("task1", "cap_a", recs, 0.8, true, false)

	famKey := familyKey(recs)
	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.VerifiedSampleCount != 1 {
		t.Fatalf("VerifiedSampleCount = %d, want 1 (all steps verified)", fs.VerifiedSampleCount)
	}
}

func TestUpsertTaskPattern_VerifiedSampleCount_NoneVerified(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok", Verified: false},
	}

	es.upsertTaskPattern("task1", "cap_a", recs, 0.8, true, false)

	famKey := familyKey(recs)
	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.VerifiedSampleCount != 0 {
		t.Fatalf("VerifiedSampleCount = %d, want 0 (no steps verified)", fs.VerifiedSampleCount)
	}
}

func TestUpsertTaskPattern_VerifiedSampleCount_MixedVerification(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	// One verified, one unverified → run is NOT verified
	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok", Verified: true, Evidence: "trace_recorded",
			Trace: []ToolInteraction{{ToolName: "read_file"}}},
		{RoleID: "r2", Capability: "cap_b", Status: "ok", Verified: false},
	}

	es.upsertTaskPattern("task1", "cap_a+cap_b", recs, 0.7, true, false)

	famKey := familyKey(recs)
	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.VerifiedSampleCount != 0 {
		t.Fatalf("VerifiedSampleCount = %d, want 0 (mixed: one verified + one unverified → run unverified)", fs.VerifiedSampleCount)
	}
}

func TestUpsertTaskPattern_VerifiedSampleCount_SkippedStepsIgnored(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	// Skipped steps don't need Verified=true; only non-skipped steps matter
	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok", Verified: true, Evidence: "trace_recorded",
			Trace: []ToolInteraction{{ToolName: "read_file"}}},
		{RoleID: "r2", Capability: "cap_b", Status: "skipped", Verified: false},
	}

	es.upsertTaskPattern("task1", "cap_a", recs, 0.8, true, false)

	famKey := familyKey(recs)
	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.VerifiedSampleCount != 1 {
		t.Fatalf("VerifiedSampleCount = %d, want 1 (skipped step doesn't need verification)", fs.VerifiedSampleCount)
	}
}

func TestUpsertTaskPattern_VerifiedSampleCount_Accumulation(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	// Run 1: verified
	recsVerified := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok", Verified: true, Evidence: "trace_recorded",
			Trace: []ToolInteraction{{ToolName: "read_file"}}},
	}
	// Run 2: unverified (same family, different sequence key)
	recsUnverified := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok", Verified: false,
			Trace: []ToolInteraction{{ToolName: "web_search"}}},
	}
	// Run 3: verified again
	recsVerified2 := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok", Verified: true, Evidence: "trace_recorded",
			Trace: []ToolInteraction{{ToolName: "read_file"}, {ToolName: "write_file"}}},
	}

	es.upsertTaskPattern("task1", "cap_a", recsVerified, 0.8, true, false)
	es.upsertTaskPattern("task2", "cap_a", recsUnverified, 0.6, true, false)
	es.upsertTaskPattern("task3", "cap_a", recsVerified2, 0.9, true, false)

	famKey := familyKey(recsVerified)
	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.SampleCount != 3 {
		t.Fatalf("SampleCount = %d, want 3", fs.SampleCount)
	}
	if fs.VerifiedSampleCount != 2 {
		t.Fatalf("VerifiedSampleCount = %d, want 2 (runs 1 and 3 verified, run 2 unverified)", fs.VerifiedSampleCount)
	}
}

func TestUpsertTaskPattern_VerifiedSampleCount_TaskPatternPersisted(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	// Same sequence key, two runs: one verified, one unverified
	recsVerified := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok", Verified: true, Evidence: "trace_recorded",
			Trace: []ToolInteraction{{ToolName: "read_file"}}},
	}
	recsUnverified := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok", Verified: false,
			Trace: []ToolInteraction{{ToolName: "read_file"}}},
	}

	es.upsertTaskPattern("task1", "cap_a", recsVerified, 0.8, true, false)
	es.upsertTaskPattern("task2", "cap_a", recsUnverified, 0.6, true, false)

	// Find the TaskPattern (same SequenceKey → merged into one)
	for _, p := range es.TaskPatterns {
		if p.SequenceKey != "" {
			if p.SampleCount != 2 {
				t.Fatalf("TaskPattern SampleCount = %d, want 2", p.SampleCount)
			}
			if p.VerifiedSampleCount != 1 {
				t.Fatalf("TaskPattern VerifiedSampleCount = %d, want 1 (only first run was verified)", p.VerifiedSampleCount)
			}
			return
		}
	}
	t.Fatal("no TaskPattern with SequenceKey found")
}

func TestRebuildFamilyKeyStats_VerifiedSampleCount(t *testing.T) {
	// Simulate loaded-from-disk TaskPatterns with VerifiedSampleCount
	es := &ExperienceStore{
		TaskPatterns: make(map[string]TaskPattern),
	}

	famKey := "cap_a|r1"

	es.TaskPatterns["pat_1"] = TaskPattern{
		ID:                  "pat_1",
		FamilyKey:           famKey,
		SampleCount:         5,
		AvgConfidence:       0.8,
		AvgTurnsUsed:        10,
		VerifiedSampleCount: 3,
	}
	es.TaskPatterns["pat_2"] = TaskPattern{
		ID:                  "pat_2",
		FamilyKey:           famKey,
		SampleCount:         2,
		AvgConfidence:       0.6,
		AvgTurnsUsed:        8,
		VerifiedSampleCount: 0,
	}

	es.rebuildFamilyKeyStatsLocked()

	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.SampleCount != 7 {
		t.Fatalf("SampleCount = %d, want 7 (5+2)", fs.SampleCount)
	}
	if fs.VerifiedSampleCount != 3 {
		t.Fatalf("VerifiedSampleCount = %d, want 3 (3+0)", fs.VerifiedSampleCount)
	}
}

func TestRebuildFamilyKeyStats_VerifiedSampleCount_ZeroWhenAbsent(t *testing.T) {
	// Legacy TaskPatterns without VerifiedSampleCount (pre-P1 data)
	es := &ExperienceStore{
		TaskPatterns: make(map[string]TaskPattern),
	}

	famKey := "cap_a|r1"

	es.TaskPatterns["pat_1"] = TaskPattern{
		ID:            "pat_1",
		FamilyKey:     famKey,
		SampleCount:   3,
		AvgConfidence: 0.8,
	}

	es.rebuildFamilyKeyStatsLocked()

	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.VerifiedSampleCount != 0 {
		t.Fatalf("VerifiedSampleCount = %d, want 0 (legacy data has no verified samples)", fs.VerifiedSampleCount)
	}
}
