package store

import (
	"testing"
)

// Tests for P2 auto-eviction: ConsecutiveFailures accumulation, reset, and
// hold behavior in upsertTaskPattern.
// Spec: docs/completed/2026-11-27/MEMORY_TWO_TIER_KEY.md §5 (P2)

func TestUpsertTaskPattern_ConsecutiveFailures_AttributableFailureIncrements(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "failed",
			Trace: []ToolInteraction{{ToolName: "tool_a"}}},
	}

	es.upsertTaskPattern("task1", "cap_a", recs, 0.3, false, true)
	famKey := familyKey(recs)
	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.ConsecutiveFailures != 1 {
		t.Fatalf("after first attributable failure, ConsecutiveFailures = %d, want 1", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("task2", "cap_a", recs, 0.2, false, true)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 2 {
		t.Fatalf("after second attributable failure, ConsecutiveFailures = %d, want 2", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("task3", "cap_a", recs, 0.1, false, true)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 3 {
		t.Fatalf("after third attributable failure, ConsecutiveFailures = %d, want 3", fs.ConsecutiveFailures)
	}
}

func TestUpsertTaskPattern_ConsecutiveFailures_SuccessResets(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok",
			Trace: []ToolInteraction{{ToolName: "tool_a"}}},
	}

	es.upsertTaskPattern("task1", "cap_a", recs, 0.3, false, true)
	es.upsertTaskPattern("task2", "cap_a", recs, 0.2, false, true)
	famKey := familyKey(recs)
	fs := es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 2 {
		t.Fatalf("after two attributable failures, ConsecutiveFailures = %d, want 2", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("task3", "cap_a", recs, 0.9, true, false)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 0 {
		t.Fatalf("after success, ConsecutiveFailures = %d, want 0 (reset)", fs.ConsecutiveFailures)
	}
}

func TestUpsertTaskPattern_ConsecutiveFailures_InfraFailureHolds(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "failed",
			Trace: []ToolInteraction{{ToolName: "tool_a"}}},
	}

	es.upsertTaskPattern("task1", "cap_a", recs, 0.3, false, true)
	famKey := familyKey(recs)
	fs := es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 1 {
		t.Fatalf("after first attributable failure, ConsecutiveFailures = %d, want 1", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("task2", "cap_a", recs, 0.2, false, false)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 1 {
		t.Fatalf("after infra failure (!success && !attributable), ConsecutiveFailures = %d, want 1 (held)", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("task3", "cap_a", recs, 0.2, false, false)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 1 {
		t.Fatalf("after second infra failure, ConsecutiveFailures = %d, want 1 (still held)", fs.ConsecutiveFailures)
	}
}

func TestUpsertTaskPattern_ConsecutiveFailures_FirstFailureInitializes(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "failed",
			Trace: []ToolInteraction{{ToolName: "tool_a"}}},
	}

	es.upsertTaskPattern("task1", "cap_a", recs, 0.3, false, true)
	famKey := familyKey(recs)
	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.ConsecutiveFailures != 1 {
		t.Fatalf("new family with attributable failure should initialize ConsecutiveFailures = 1, got %d", fs.ConsecutiveFailures)
	}
}

func TestUpsertTaskPattern_ConsecutiveFailures_NewFamilyWithSuccessStartsZero(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok",
			Trace: []ToolInteraction{{ToolName: "tool_a"}}},
	}

	es.upsertTaskPattern("task1", "cap_a", recs, 0.9, true, false)
	famKey := familyKey(recs)
	fs := es.familyKeyStats[famKey]
	if fs == nil {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.ConsecutiveFailures != 0 {
		t.Fatalf("new family with success should have ConsecutiveFailures = 0, got %d", fs.ConsecutiveFailures)
	}
}

func TestUpsertTaskPattern_ConsecutiveFailures_MixedSequence(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	recs := []StepRecord{
		{RoleID: "r1", Capability: "cap_a", Status: "ok",
			Trace: []ToolInteraction{{ToolName: "tool_a"}}},
	}
	famKey := familyKey(recs)

	es.upsertTaskPattern("t1", "cap_a", recs, 0.9, true, false)
	fs := es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 0 {
		t.Fatalf("step 1 (success): ConsecutiveFailures = %d, want 0", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("t2", "cap_a", recs, 0.3, false, true)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 1 {
		t.Fatalf("step 2 (attributable failure): ConsecutiveFailures = %d, want 1", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("t3", "cap_a", recs, 0.2, false, false)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 1 {
		t.Fatalf("step 3 (infra failure): ConsecutiveFailures = %d, want 1 (held)", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("t4", "cap_a", recs, 0.3, false, true)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 2 {
		t.Fatalf("step 4 (attributable failure): ConsecutiveFailures = %d, want 2", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("t5", "cap_a", recs, 0.3, false, true)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 3 {
		t.Fatalf("step 5 (attributable failure): ConsecutiveFailures = %d, want 3", fs.ConsecutiveFailures)
	}

	es.upsertTaskPattern("t6", "cap_a", recs, 0.9, true, false)
	fs = es.familyKeyStats[famKey]
	if fs.ConsecutiveFailures != 0 {
		t.Fatalf("step 6 (success after 3 failures): ConsecutiveFailures = %d, want 0 (reset)", fs.ConsecutiveFailures)
	}
}
