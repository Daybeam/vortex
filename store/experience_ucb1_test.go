package store

import (
	"context"
	"testing"
)

// TestSelectSkill_UCB1_UnvisitedAlwaysWinsOverVisited is the deterministic
// successor to the old epsilon-greedy "eventually explores" test (which
// relied on 50 iterations and a real chance of missing the untried
// candidate). UCB1's forced-exploration tier means this is now guaranteed
// on the very first call, not just probable over many.
func TestSelectSkill_UCB1_UnvisitedAlwaysWinsOverVisited(t *testing.T) {
	es := newTestStore(t)
	es.RoutingMatrix["role_a"] = map[string]map[string]map[string]*RouteWeight{
		"model_v1": {"cap_x": {"skill_established": {TotalRuns: 50, SuccessCount: 50, Weight: 0.95}}},
	}

	got := es.SelectSkill(context.Background(), []string{"skill_established", "skill_never_tried"}, "cap_x", "role_a", "model_v1", 0.1)
	if got != "skill_never_tried" {
		t.Fatalf("expected the untried candidate to win outright (UCB1 forced exploration), got %q", got)
	}
}

// TestSelectSkill_UCB1_ExplorationBonusScalesWithFewerSamples verifies the
// actual UCB1 property that motivated replacing flat epsilon: at an equal
// observed average score, the candidate with far fewer samples wins because
// its exploration bonus (small denominator) is much larger.
func TestSelectSkill_UCB1_ExplorationBonusScalesWithFewerSamples(t *testing.T) {
	es := newTestStore(t)
	es.RoutingMatrix["role_a"] = map[string]map[string]map[string]*RouteWeight{
		"model_v1": {"cap_x": {
			"skill_many_samples": {TotalRuns: 1000, SuccessCount: 900, Weight: 0.90},
			"skill_few_samples":  {TotalRuns: 2, SuccessCount: 2, Weight: 0.90},
		}},
	}

	got := es.SelectSkill(context.Background(), []string{"skill_many_samples", "skill_few_samples"}, "cap_x", "role_a", "model_v1", 0.5)
	if got != "skill_few_samples" {
		t.Fatalf("expected the low-sample candidate to win via UCB1 exploration bonus at equal score, got %q", got)
	}
}

// TestExperienceScore_SimilaritySmoothing_BorrowsFromSimilarCapability is
// the core regression test for the 2026-08-07 cold-start fix: a capability
// phrasing that doesn't exactly match any recorded key must still borrow a
// damped prior from a textually similar recorded phrasing for the SAME
// skill, rather than cold-starting at (0, 0).
func TestExperienceScore_SimilaritySmoothing_BorrowsFromSimilarCapability(t *testing.T) {
	es := newTestStore(t)
	es.SkillAffinities["fetch market quote data:skill_fetch_market_data"] = &SkillAffinity{
		BaseCapability: "fetch market quote data", AddedSkill: "skill_fetch_market_data",
		ConfidenceDelta: 0.4, SampleCount: 10,
	}

	score, samples := es.experienceScore("role_a", "model_v1", "pull market quote information", "skill_fetch_market_data")
	if samples != 0 {
		t.Fatalf("expected realSampleCount 0 for a borrowed/smoothed score (no direct experience with this exact phrasing), got %d", samples)
	}
	wantScore := 0.4 * similaritySmoothingDamp
	if score != wantScore {
		t.Fatalf("expected damped borrowed score %.4f, got %.4f", wantScore, score)
	}
}

// TestExperienceScore_SimilaritySmoothing_RequiresMinimumOverlap ensures a
// barely-related capability phrasing does NOT borrow a prior -- the same
// false-positive concern that has caused real regressions elsewhere in this
// codebase (tool_router.go, hub.go's FindSkillByCapability) if similarity
// matching is too permissive.
func TestExperienceScore_SimilaritySmoothing_RequiresMinimumOverlap(t *testing.T) {
	es := newTestStore(t)
	es.SkillAffinities["fetch market quote data:skill_fetch_market_data"] = &SkillAffinity{
		BaseCapability: "fetch market quote data", AddedSkill: "skill_fetch_market_data",
		ConfidenceDelta: 0.9, SampleCount: 10,
	}

	// Shares only "data" (a real, non-stopword token, but a single word out
	// of a much longer, otherwise-unrelated phrase) -- must not borrow.
	score, samples := es.experienceScore("role_a", "model_v1", "render a formatted PDF export of the annual report data", "skill_fetch_market_data")
	if score != 0 || samples != 0 {
		t.Fatalf("expected no smoothing for a barely-related phrasing, got score=%.4f samples=%d", score, samples)
	}
}

// TestExperienceScore_SimilaritySmoothing_DoesNotCrossSkillBoundary ensures
// smoothing only borrows from entries for the SAME skillID, not from an
// unrelated skill that happens to share a similar capability phrasing.
func TestExperienceScore_SimilaritySmoothing_DoesNotCrossSkillBoundary(t *testing.T) {
	es := newTestStore(t)
	es.SkillAffinities["fetch market quote data:skill_OTHER_skill"] = &SkillAffinity{
		BaseCapability: "fetch market quote data", AddedSkill: "skill_OTHER_skill",
		ConfidenceDelta: 0.9, SampleCount: 10,
	}

	score, samples := es.experienceScore("role_a", "model_v1", "fetch market quote data", "skill_fetch_market_data")
	if score != 0 || samples != 0 {
		t.Fatalf("expected no smoothing across a different skillID, got score=%.4f samples=%d", score, samples)
	}
}
