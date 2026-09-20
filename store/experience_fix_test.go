package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *ExperienceStore {
	t.Helper()
	// os.MkdirTemp instead of t.TempDir(): NewExperienceStore spawns an async
	// persistence goroutine that can still hold file handles when the test
	// returns, which makes t.TempDir's auto-cleanup fail on Windows
	// ("directory is not empty"). Leaving the dir for the OS temp cleaner
	// avoids that race (same pattern as core/delegation_mode_test.go).
	dir, err := os.MkdirTemp("", "vortex-store-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	es, err := NewExperienceStore(dir, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore: %v", err)
	}
	return es
}

func TestSelectSkill_ExploitsHigherRoutingWeight(t *testing.T) {
	es := newTestStore(t)
	es.RoutingMatrix["role_a"] = map[string]map[string]map[string]*RouteWeight{
		"model_v1": {
			"cap_x": {
				"skill_low":  {TotalRuns: 5, SuccessCount: 1, Weight: 0.2},
				"skill_high": {TotalRuns: 5, SuccessCount: 5, Weight: 0.9},
			},
		},
	}
	got := es.SelectSkill(context.Background(), []string{"skill_low", "skill_high"}, "cap_x", "role_a", "model_v1", 0.0)
	if got != "skill_high" {
		t.Fatalf("expected skill_high (higher weight), got %q", got)
	}
}

func TestSelectSkill_ColdStartReturnsFirst(t *testing.T) {
	es := newTestStore(t)
	got := es.SelectSkill(context.Background(), []string{"only_one", "second"}, "cap_unknown", "role_unknown", "model_unknown", 0.0)
	if got != "only_one" {
		t.Fatalf("cold start expected first candidate, got %q", got)
	}
}

func TestSelectSkill_EpsilonOneAlwaysExplores(t *testing.T) {
	es := newTestStore(t)
	es.RoutingMatrix["role_a"] = map[string]map[string]map[string]*RouteWeight{
		"model_v1": {
			"cap_x": {"skill_high": {TotalRuns: 5, SuccessCount: 5, Weight: 0.9}},
		},
	}
	seenOther := false
	for i := 0; i < 50; i++ {
		got := es.SelectSkill(context.Background(), []string{"skill_high", "skill_never_scored"}, "cap_x", "role_a", "model_v1", 1.0)
		if got == "skill_never_scored" {
			seenOther = true
			break
		}
	}
	if !seenOther {
		t.Fatalf("epsilon=1.0 should eventually explore the non-best candidate")
	}
}

func TestQueryDecisionAdvice_MajorityVoteAndNoDataCase(t *testing.T) {
	es := newTestStore(t)
	if got := es.QueryDecisionAdvice(context.Background(), "low_confidence", "role_a"); got != nil {
		t.Fatalf("expected nil with no data, got %+v", got)
	}

	c1 := 0.9
	c2 := 0.8
	es.DecisionOutcomes = []*DecisionOutcome{
		{DecisionType: "low_confidence", RoleID: "role_a", Choice: "skip", Resolved: true, FinalConfidence: &c1, Timestamp: time.Now()},
		{DecisionType: "low_confidence", RoleID: "role_a", Choice: "skip", Resolved: true, FinalConfidence: &c2, Timestamp: time.Now()},
		{DecisionType: "low_confidence", RoleID: "role_a", Choice: "abort", Resolved: true, Timestamp: time.Now()},
		{DecisionType: "low_confidence", RoleID: "role_a", Choice: "abort", Resolved: false, Timestamp: time.Now()}, // unresolved, must be ignored
		{DecisionType: "low_confidence", RoleID: "role_b", Choice: "abort", Resolved: true, Timestamp: time.Now()},  // wrong role, must be ignored
	}

	got := es.QueryDecisionAdvice(context.Background(), "low_confidence", "role_a")
	if got == nil {
		t.Fatalf("expected non-nil advice")
	}
	if got.Choice != "skip" {
		t.Fatalf("expected majority choice 'skip', got %q", got.Choice)
	}
	if got.FinalConfidence == nil || *got.FinalConfidence < 0.84 || *got.FinalConfidence > 0.86 {
		t.Fatalf("expected averaged confidence ~0.85, got %+v", got.FinalConfidence)
	}
}

func TestQuerySkillRecommendations_ThresholdAndSeedTrust(t *testing.T) {
	es := newTestStore(t)
	es.SkillAffinities["cap_x:skill_strong"] = &SkillAffinity{
		BaseCapability: "cap_x", AddedSkill: "skill_strong", ConfidenceDelta: 0.2, SampleCount: 5,
	}
	es.SkillAffinities["cap_x:skill_noisy"] = &SkillAffinity{
		BaseCapability: "cap_x", AddedSkill: "skill_noisy", ConfidenceDelta: 0.3, SampleCount: 1, // below trust threshold, non-seed
	}
	es.SkillAffinities["cap_x:skill_seed"] = &SkillAffinity{
		BaseCapability: "cap_x", AddedSkill: "skill_seed", ConfidenceDelta: 0.1, SampleCount: 1, IsSeed: true,
	}
	es.SkillAffinities["cap_y:skill_other_cap"] = &SkillAffinity{
		BaseCapability: "cap_y", AddedSkill: "skill_other_cap", ConfidenceDelta: 0.5, SampleCount: 5,
	}

	got := es.QuerySkillRecommendations(context.Background(), "cap_x", 0.8)
	if len(got) != 2 {
		t.Fatalf("expected 2 qualifying recommendations (strong + seed), got %v", got)
	}
	if got[0] != "skill_strong" {
		t.Fatalf("expected skill_strong ranked first (higher delta), got %v", got)
	}
}

func TestQueryRelevantSkills_OverlapAndNoFalsePositive(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["gs1"] = &GeneratedSkill{
		ID: "gs1", Description: "take a screenshot of the current browser page", Capability: "browser_automation", SuccessRate: 0.9,
	}
	es.GeneratedSkills["gs2"] = &GeneratedSkill{
		ID: "gs2", Description: "run a database migration script", Capability: "devops", SuccessRate: 0.5,
	}

	got := es.QueryRelevantSkills(context.Background(), "please take a screenshot of the page", 3)
	if len(got) != 1 || got[0] != "gs1" {
		t.Fatalf("expected only gs1 to match, got %v", got)
	}

	gotNone := es.QueryRelevantSkills(context.Background(), "what is the capital of france", 3)
	if len(gotNone) != 0 {
		t.Fatalf("expected no matches for unrelated task text, got %v", gotNone)
	}
}

func TestRecordTaskCompletion_PopulatesDecisionOutcomes(t *testing.T) {
	es := newTestStore(t)
	decisions := []map[string]any{
		{"decision_type": "low_confidence", "role_id": "role_a", "choice": "retry"},
	}
	if err := es.RecordTaskCompletion(context.Background(), "task_1", nil, 0.95, decisions, true, false); err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // Give background persistence time to finish
	if len(es.DecisionOutcomes) != 1 {
		t.Fatalf("expected 1 recorded decision outcome, got %d", len(es.DecisionOutcomes))
	}
	got := es.DecisionOutcomes[0]
	if got.Choice != "retry" || got.RoleID != "role_a" || !got.Resolved {
		t.Fatalf("unexpected recorded outcome: %+v", got)
	}
	if got.FinalConfidence == nil || *got.FinalConfidence != 0.95 {
		t.Fatalf("expected final_confidence 0.95, got %+v", got.FinalConfidence)
	}
}
