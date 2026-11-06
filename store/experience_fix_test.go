package store

import (
	"context"
	"fmt"
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
	// DecisionOutcomes is appended synchronously under es.Mu before
	// RecordTaskCompletion returns. Drain the background persist goroutine
	// (deterministic wait, replaces a flaky time.Sleep) then read under the
	// lock so the assertion cannot race the persister.
	es.WaitAsyncSaves()
	es.Mu.RLock()
	gotCount := len(es.DecisionOutcomes)
	var got *DecisionOutcome
	if gotCount == 1 {
		got = es.DecisionOutcomes[0]
	}
	es.Mu.RUnlock()
	if gotCount != 1 {
		t.Fatalf("expected 1 recorded decision outcome, got %d", gotCount)
	}
	if got.Choice != "retry" || got.RoleID != "role_a" || !got.Resolved {
		t.Fatalf("unexpected recorded outcome: %+v", got)
	}
	if got.FinalConfidence == nil || *got.FinalConfidence != 0.95 {
		t.Fatalf("expected final_confidence 0.95, got %+v", got.FinalConfidence)
	}
}

// ── Defect 5 regression tests (JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §6) ──

// TestQueryRelevantSkills_RealJITNoiseFiltered reproduces the exact noise
// observed in production: two task-level JIT skills with long descriptions
// getting injected into unrelated tasks via 1-token lexical overlap.
//
// Before fix: overlap=1 → score=1+SuccessRate*0.5 → passes.
// After fix: overlapRatio=1/40=0.025 < threshold=0.45 → filtered.
//
// See: docs/completed/2026-11-05/JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §1.2
func TestQueryRelevantSkills_RealJITNoiseFiltered(t *testing.T) {
	es := newTestStore(t)

	// Recreate jit_21caf0fe: ~40 token business analysis task
	es.GeneratedSkills["jit_21caf0fe"] = &GeneratedSkill{
		ID:          "jit_21caf0fe",
		Description: "Research the current market trends competitors and user pain points for business logic validation question I have posted eleven genuine substantive domain expert comments on trending Dev articles over ten sessions Results eleven reactions total on my four articles but zero profile click throughs from those eleven comments and zero article view lift",
		Capability:  "analyze",
		SuccessRate: 0.24,
		UsageCount:  0,
	}

	// Recreate jit_eacb615c: ~25 token WeChat scraping task
	es.GeneratedSkills["jit_eacb615c"] = &GeneratedSkill{
		ID:          "jit_eacb615c",
		Description: "Use ghost driver to fetch WeChat article content by connecting browser in attached mode then navigating to the article URL waiting for page load and extracting markdown text from the page body content",
		Capability:  "browser automation scraping",
		SuccessRate: 0.28,
		UsageCount:  0,
	}

	// Query with an unrelated task that shares 1-2 tokens ("research", "use")
	got := es.QueryRelevantSkills(context.Background(), "security audit for authentication flow verification", 5)
	for _, id := range got {
		if id == "jit_21caf0fe" || id == "jit_eacb615c" {
			t.Errorf("noise skill %q should be filtered by specificity threshold, got results: %v", id, got)
		}
	}
}

// TestQueryRelevantSkills_SpecificityThreshold verifies that broad skills
// (few tokens) pass with low overlap, while specific skills (many tokens)
// are filtered unless overlap is high.
//
// See: docs/completed/2026-11-05/JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §2.3
func TestQueryRelevantSkills_SpecificityThreshold(t *testing.T) {
	es := newTestStore(t)

	// Broad skill: 3 tokens → specificity=0.15, threshold=0.195
	es.GeneratedSkills["broad"] = &GeneratedSkill{
		ID:          "broad",
		Description: "security audit tool",
		Capability:  "security",
		SuccessRate: 0.5,
		UsageCount:  5,
	}

	// Specific skill: 25 tokens → specificity=1.0, threshold=0.45
	es.GeneratedSkills["specific"] = &GeneratedSkill{
		ID:          "specific",
		Description: "check race condition in scheduler dag exec step pending reset bug fix for concurrent task dispatch when delegation mode is enabled and multiple steps run in parallel with shared state",
		Capability:  "code analysis",
		SuccessRate: 0.9,
		UsageCount:  3,
	}

	// Query shares 2 tokens with broad ("security", "audit")
	// overlapRatio = 2/max(4,4) = 0.5 >= 0.195 → passes
	got := es.QueryRelevantSkills(context.Background(), "security audit verification check", 5)
	foundBroad := false
	for _, id := range got {
		if id == "broad" {
			foundBroad = true
		}
	}
	if !foundBroad {
		t.Errorf("broad skill should pass with 2-token overlap, got: %v", got)
	}

	// Query shares 2 tokens with specific ("check", "race")
	// overlapRatio = 2/max(5,25) = 0.08 < 0.45 → filtered
	for _, id := range got {
		if id == "specific" {
			t.Errorf("specific skill should be filtered (ratio=0.08 < threshold=0.45), got: %v", got)
		}
	}
}

// TestQueryRelevantSkills_ConfidenceWeightedSuccessRate verifies that
// unproven skills (UsageCount=0) get no SuccessRate boost, while proven
// skills get a small tiebreaker.
//
// See: docs/completed/2026-11-05/JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §2.4
func TestQueryRelevantSkills_ConfidenceWeightedSuccessRate(t *testing.T) {
	es := newTestStore(t)

	// Two skills with same description but different UsageCount.
	// The proven one should rank higher due to confidence-weighted boost.
	desc := "code review static analysis"
	es.GeneratedSkills["unproven"] = &GeneratedSkill{
		ID:          "unproven",
		Description: desc,
		Capability:  "review",
		SuccessRate: 1.0, // high but unproven
		UsageCount:  0,
	}
	es.GeneratedSkills["proven"] = &GeneratedSkill{
		ID:          "proven",
		Description: desc,
		Capability:  "review",
		SuccessRate: 0.8,
		UsageCount:  10, // proven
	}

	got := es.QueryRelevantSkills(context.Background(), "code review analysis", 2)
	if len(got) < 2 {
		t.Fatalf("expected both skills to pass, got: %v", got)
	}
	// Proven should rank first (relevance + 0.8*1.0*0.1 = relevance + 0.08)
	// Unproven should rank second (relevance + 1.0*0.0*0.1 = relevance + 0.0)
	if got[0] != "proven" {
		t.Errorf("proven skill should rank first, got order: %v", got)
	}
}

// mockEmbeddingClient implements IEmbeddingClient for testing.
type mockEmbeddingClient struct {
	vectors map[string][]float32
	err     error
}

func (m *mockEmbeddingClient) EmbedWithModel(ctx context.Context, text string) ([]float32, string, error) {
	if m.err != nil {
		return nil, "", m.err
	}
	if vec, ok := m.vectors[text]; ok {
		return vec, "mock-model", nil
	}
	return nil, "", fmt.Errorf("no embedding for: %s", text)
}

// TestQueryRelevantSkills_EmbeddingDegradation verifies that when the
// embedding client returns an error, the system falls back to lexical
// scoring without crashing.
//
// See: docs/completed/2026-11-05/JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §3
func TestQueryRelevantSkills_EmbeddingDegradation(t *testing.T) {
	es := newTestStore(t)

	es.GeneratedSkills["gs1"] = &GeneratedSkill{
		ID:          "gs1",
		Description: "take a screenshot of the current browser page",
		Capability:  "browser automation",
		SuccessRate: 0.9,
		UsageCount:  5,
	}

	// Wire a failing embedding client
	es.SetEmbeddingClient(&mockEmbeddingClient{err: fmt.Errorf("service unavailable")})

	// Should still work via lexical fallback
	got := es.QueryRelevantSkills(context.Background(), "take a screenshot of the page", 3)
	if len(got) != 1 || got[0] != "gs1" {
		t.Fatalf("lexical fallback should still return gs1, got: %v", got)
	}
}

// TestQueryRelevantSkills_SemanticWithEmbeddings verifies that when
// embeddings are available, cosine similarity is used for scoring.
//
// See: docs/completed/2026-11-05/JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §2.2
func TestQueryRelevantSkills_SemanticWithEmbeddings(t *testing.T) {
	es := newTestStore(t)

	// Skill with short description (low specificity threshold)
	desc := "security audit tool"
	es.GeneratedSkills["gs1"] = &GeneratedSkill{
		ID:          "gs1",
		Description: desc,
		Capability:  "security",
		SuccessRate: 0.5,
		UsageCount:  5,
	}

	query := "security audit verification"

	// Set up embeddings: query and skill have high cosine similarity
	// (identical vectors → cosine = 1.0)
	vec := []float32{1.0, 0.0, 0.0}
	es.SetEmbeddingClient(&mockEmbeddingClient{
		vectors: map[string][]float32{
			query: vec,
			desc:  vec,
		},
	})

	got := es.QueryRelevantSkills(context.Background(), query, 3)
	if len(got) != 1 || got[0] != "gs1" {
		t.Fatalf("semantic match should return gs1, got: %v", got)
	}
}
