package store

import (
	"context"
	"testing"
	"time"
)

// TestRecordTaskCompletion_AttributableFailureAppliesLightPenalty is the
// core regression test for the 2026-08-07 light credit-assignment feature:
// when a task fails for a reason attributable to skill/role choice, every
// skill used anywhere in the task should get a small additional negative
// nudge, on top of whatever their own per-step Confidence already recorded.
func TestRecordTaskCompletion_AttributableFailureAppliesLightPenalty(t *testing.T) {
	es := newTestStore(t)
	records := []StepRecord{
		{RoleID: "role_a", ModelID: "model_v1", Skills: []string{"skill_x"}, Capability: "cap_a", Confidence: 0.8, Status: "ok"},
	}

	if err := es.RecordTaskCompletion(context.Background(), "task_1", records, 0.2, nil, false, true); err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	sa := es.SkillAffinities["cap_a:skill_x"]
	if sa == nil {
		t.Fatal("expected a SkillAffinity entry to exist")
	}
	// SampleCount should be 2: one from updateSkillAffinities (the step's
	// own local 0.8 confidence), one from the additional task-level penalty.
	if sa.SampleCount != 2 {
		t.Fatalf("expected SampleCount 2 (local update + penalty), got %d", sa.SampleCount)
	}
}

// TestRecordTaskCompletion_NonAttributableFailureSkipsPenalty verifies the
// exact scenario Connor raised: a failure caused by something outside the
// skill's control (rate limiting, network, etc.) must NOT apply the
// additional penalty -- only the step's own normal local update happens.
func TestRecordTaskCompletion_NonAttributableFailureSkipsPenalty(t *testing.T) {
	es := newTestStore(t)
	records := []StepRecord{
		{RoleID: "role_a", ModelID: "model_v1", Skills: []string{"skill_x"}, Capability: "cap_a", Confidence: 0.8, Status: "ok"},
	}

	if err := es.RecordTaskCompletion(context.Background(), "task_1", records, 0.2, nil, false, false); err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	sa := es.SkillAffinities["cap_a:skill_x"]
	if sa == nil {
		t.Fatal("expected a SkillAffinity entry to exist")
	}
	if sa.SampleCount != 1 {
		t.Fatalf("expected SampleCount 1 (local update only, no penalty), got %d", sa.SampleCount)
	}
}

// TestRecordTaskCompletion_SuccessNeverAppliesPenaltyEvenIfAttributableTrue
// guards against a caller mistake: attributableFailure is only meaningful
// when success is false; it must be a no-op if success is true regardless
// of what a (buggy) caller passes for attributableFailure.
func TestRecordTaskCompletion_SuccessNeverAppliesPenaltyEvenIfAttributableTrue(t *testing.T) {
	es := newTestStore(t)
	records := []StepRecord{
		{RoleID: "role_a", ModelID: "model_v1", Skills: []string{"skill_x"}, Capability: "cap_a", Confidence: 0.9, Status: "ok"},
	}

	if err := es.RecordTaskCompletion(context.Background(), "task_1", records, 0.9, nil, true, true); err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	sa := es.SkillAffinities["cap_a:skill_x"]
	if sa.SampleCount != 1 {
		t.Fatalf("expected SampleCount 1 (success path never penalizes), got %d", sa.SampleCount)
	}
}

// TestApplyTaskLevelPenalty_DoesNotDoubleCountSameSkillTwiceInOneTask
// ensures a skill used in two different steps of the same failed task only
// gets the supplementary penalty once, not once per step it appeared in.
func TestApplyTaskLevelPenalty_DoesNotDoubleCountSameSkillTwiceInOneTask(t *testing.T) {
	es := newTestStore(t)
	records := []StepRecord{
		{RoleID: "role_a", ModelID: "model_v1", Skills: []string{"skill_x"}, Capability: "cap_a", Confidence: 0.7, Status: "ok"},
		{RoleID: "role_a", ModelID: "model_v1", Skills: []string{"skill_x"}, Capability: "cap_a", Confidence: 0.6, Status: "ok"},
	}

	if err := es.RecordTaskCompletion(context.Background(), "task_1", records, 0.2, nil, false, true); err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	sa := es.SkillAffinities["cap_a:skill_x"]
	// 2 local updates (one per step) + 1 penalty (deduped across the task) = 3.
	if sa.SampleCount != 3 {
		t.Fatalf("expected SampleCount 3 (2 local + 1 deduped penalty), got %d", sa.SampleCount)
	}
}
