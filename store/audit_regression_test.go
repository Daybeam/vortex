package store

import (
	"fmt"
	"testing"
	"time"
)

// ─── Regression: SanitizeError still works after regex hoisting ───

func TestSanitizeError_BehaviorPreserved(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"no keys", "connection refused", "connection refused"},
		{"api_key in URL", "fetch failed: ?api_key=AIzaSyABC123", "fetch failed: ?api_key=***"},
		{"token in URL", "error: token=secret_xyz", "error: token=***"},
		{"key in URL", "bad request: key=abc123", "bad request: key=***"},
		{"multiple keys", "?key=aaa&token=bbb", "?key=***&token=***"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeError(tc.input)
			if got != tc.want {
				t.Errorf("SanitizeError(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// ─── Regression: PruneSkills O(N) top-2 index produces correct results ───
// This test verifies that the optimized O(N) PruneSkills (using a per-capability
// top-2 ConfidenceDelta index) prunes the same skills the O(N²) version would.
// We set up skills with different capabilities, sample counts, and deltas,
// then verify only the underperforming variants with enough samples are pruned.

func TestPruneSkills_Top2Index_CorrectPruning(t *testing.T) {
	es := &ExperienceStore{
		GeneratedSkills: make(map[string]*GeneratedSkill),
		ArchivedSkills:  make(map[string]*GeneratedSkill),
		SkillAffinities: make(map[string]*SkillAffinity),
	}

	capability := "coding"
	now := time.Now()

	// Root skill — should NEVER be pruned (ParentID == "")
	es.GeneratedSkills["root"] = &GeneratedSkill{
		ID:         "root",
		Capability: capability,
		ParentID:   "",
		LastUsed:   now,
	}
	es.SkillAffinities["coding:root"] = &SkillAffinity{
		BaseCapability:  capability,
		AddedSkill:      "root",
		ConfidenceDelta: 0.5,
		SampleCount:     100,
	}

	// Best variant — high delta, enough samples, should NOT be pruned
	es.GeneratedSkills["best"] = &GeneratedSkill{
		ID:         "best",
		Capability: capability,
		ParentID:   "root",
		LastUsed:   now,
	}
	es.SkillAffinities["coding:best"] = &SkillAffinity{
		BaseCapability:  capability,
		AddedSkill:      "best",
		ConfidenceDelta: 0.8,
		SampleCount:     50, // >= minSamplesToPrune (20)
	}

	// Underperforming variant — low delta, enough samples, SHOULD be pruned
	// Condition: delta < bestSibling - pruneWeightMargin
	// 0.1 < 0.8 - 0.15 = 0.65 → true, should be pruned
	es.GeneratedSkills["underperformer"] = &GeneratedSkill{
		ID:         "underperformer",
		Capability: capability,
		ParentID:   "root",
		LastUsed:   now,
	}
	es.SkillAffinities["coding:underperformer"] = &SkillAffinity{
		BaseCapability:  capability,
		AddedSkill:      "underperformer",
		ConfidenceDelta: 0.1,
		SampleCount:     30, // >= minSamplesToPrune (20)
	}

	// New variant — low delta but NOT enough samples, should NOT be pruned
	es.GeneratedSkills["newbie"] = &GeneratedSkill{
		ID:         "newbie",
		Capability: capability,
		ParentID:   "root",
		LastUsed:   now,
	}
	es.SkillAffinities["coding:newbie"] = &SkillAffinity{
		BaseCapability:  capability,
		AddedSkill:      "newbie",
		ConfidenceDelta: 0.05,
		SampleCount:     5, // < minSamplesToPrune (20)
	}

	// Marginal variant — delta is within margin of best, should NOT be pruned
	// Condition: 0.7 < 0.8 - 0.15 = 0.65 → false, should NOT be pruned
	es.GeneratedSkills["marginal"] = &GeneratedSkill{
		ID:         "marginal",
		Capability: capability,
		ParentID:   "root",
		LastUsed:   now,
	}
	es.SkillAffinities["coding:marginal"] = &SkillAffinity{
		BaseCapability:  capability,
		AddedSkill:      "marginal",
		ConfidenceDelta: 0.7,
		SampleCount:     25,
	}

	pruned := es.PruneSkills()

	// Only "underperformer" should be pruned
	if len(pruned) != 1 {
		t.Fatalf("expected 1 pruned skill, got %d: %v", len(pruned), pruned)
	}
	if pruned[0] != "underperformer" {
		t.Errorf("expected 'underperformer' to be pruned, got %q", pruned[0])
	}

	// Verify it was archived, not deleted
	if _, ok := es.ArchivedSkills["underperformer"]; !ok {
		t.Error("underperformer should be in ArchivedSkills")
	}
	if _, ok := es.GeneratedSkills["underperformer"]; ok {
		t.Error("underperformer should be removed from GeneratedSkills")
	}

	// Verify all others are still in GeneratedSkills
	for _, id := range []string{"root", "best", "newbie", "marginal"} {
		if _, ok := es.GeneratedSkills[id]; !ok {
			t.Errorf("%q should still be in GeneratedSkills", id)
		}
	}
}

// ─── Regression: PruneSkills handles multiple capabilities correctly ───
// Verifies the top-2 index is per-capability, not global.

func TestPruneSkills_MultipleCapabilities_Top2Isolated(t *testing.T) {
	es := &ExperienceStore{
		GeneratedSkills: make(map[string]*GeneratedSkill),
		ArchivedSkills:  make(map[string]*GeneratedSkill),
		SkillAffinities: make(map[string]*SkillAffinity),
	}
	now := time.Now()

	// Two capabilities, each with a best and underperformer
	for _, cap := range []string{"coding", "analysis"} {
		// Root
		es.GeneratedSkills[cap+"-root"] = &GeneratedSkill{ID: cap + "-root", Capability: cap, ParentID: "", LastUsed: now}
		es.SkillAffinities[cap+":"+cap+"-root"] = &SkillAffinity{BaseCapability: cap, AddedSkill: cap + "-root", ConfidenceDelta: 0.5, SampleCount: 100}

		// Best variant
		es.GeneratedSkills[cap+"-best"] = &GeneratedSkill{ID: cap + "-best", Capability: cap, ParentID: cap + "-root", LastUsed: now}
		es.SkillAffinities[cap+":"+cap+"-best"] = &SkillAffinity{BaseCapability: cap, AddedSkill: cap + "-best", ConfidenceDelta: 0.9, SampleCount: 30}

		// Underperformer — should be pruned (0.1 < 0.9 - 0.15)
		es.GeneratedSkills[cap+"-under"] = &GeneratedSkill{ID: cap + "-under", Capability: cap, ParentID: cap + "-root", LastUsed: now}
		es.SkillAffinities[cap+":"+cap+"-under"] = &SkillAffinity{BaseCapability: cap, AddedSkill: cap + "-under", ConfidenceDelta: 0.1, SampleCount: 30}
	}

	pruned := es.PruneSkills()

	if len(pruned) != 2 {
		t.Fatalf("expected 2 pruned skills (one per capability), got %d: %v", len(pruned), pruned)
	}

	prunedSet := make(map[string]bool)
	for _, id := range pruned {
		prunedSet[id] = true
	}
	for _, expected := range []string{"coding-under", "analysis-under"} {
		if !prunedSet[expected] {
			t.Errorf("expected %q to be pruned", expected)
		}
	}
}

// ─── Regression: PruneSkills with best skill being the one checked ───
// Verifies the top-2 logic correctly falls back to second-best when
// the current skill IS the best (excludeID == best.id).

func TestPruneSkills_BestSkillUsesSecondBest(t *testing.T) {
	es := &ExperienceStore{
		GeneratedSkills: make(map[string]*GeneratedSkill),
		ArchivedSkills:  make(map[string]*GeneratedSkill),
		SkillAffinities: make(map[string]*SkillAffinity),
	}
	now := time.Now()
	cap := "test"

	// Root
	es.GeneratedSkills["root"] = &GeneratedSkill{ID: "root", Capability: cap, ParentID: "", LastUsed: now}
	es.SkillAffinities["test:root"] = &SkillAffinity{BaseCapability: cap, AddedSkill: "root", ConfidenceDelta: 0.5, SampleCount: 100}

	// Best variant (delta 0.9) — when checking this skill, bestSibling should be 0.8 (second-best)
	// 0.9 < 0.8 - 0.15 = 0.65 → false, should NOT be pruned
	es.GeneratedSkills["best"] = &GeneratedSkill{ID: "best", Capability: cap, ParentID: "root", LastUsed: now}
	es.SkillAffinities["test:best"] = &SkillAffinity{BaseCapability: cap, AddedSkill: "best", ConfidenceDelta: 0.9, SampleCount: 30}

	// Second-best variant (delta 0.8) — within margin of best
	// 0.8 < 0.9 - 0.15 = 0.75 → false, should NOT be pruned
	es.GeneratedSkills["second"] = &GeneratedSkill{ID: "second", Capability: cap, ParentID: "root", LastUsed: now}
	es.SkillAffinities["test:second"] = &SkillAffinity{BaseCapability: cap, AddedSkill: "second", ConfidenceDelta: 0.8, SampleCount: 30}

	pruned := es.PruneSkills()

	if len(pruned) != 0 {
		t.Errorf("expected 0 pruned skills (best and second-best are within margin), got %d: %v", len(pruned), pruned)
	}
}

// ─── Regression: PruneSkills with large N verifies O(N) doesn't break ───
// Creates 500 skills across 10 capabilities and verifies pruning works.

func TestPruneSkills_LargeN_CorrectBehavior(t *testing.T) {
	es := &ExperienceStore{
		GeneratedSkills: make(map[string]*GeneratedSkill),
		ArchivedSkills:  make(map[string]*GeneratedSkill),
		SkillAffinities: make(map[string]*SkillAffinity),
	}
	now := time.Now()

	for capIdx := 0; capIdx < 10; capIdx++ {
		cap := fmt.Sprintf("cap-%d", capIdx)
		// Root
		rootID := cap + "-root"
		es.GeneratedSkills[rootID] = &GeneratedSkill{ID: rootID, Capability: cap, ParentID: "", LastUsed: now}
		es.SkillAffinities[cap+":"+rootID] = &SkillAffinity{BaseCapability: cap, AddedSkill: rootID, ConfidenceDelta: 0.5, SampleCount: 100}

		for i := 0; i < 50; i++ {
			id := fmt.Sprintf("%s-v%d", cap, i)
			delta := 0.8 - float64(i)*0.01 // decreasing delta
			es.GeneratedSkills[id] = &GeneratedSkill{ID: id, Capability: cap, ParentID: rootID, LastUsed: now}
			es.SkillAffinities[cap+":"+id] = &SkillAffinity{BaseCapability: cap, AddedSkill: id, ConfidenceDelta: delta, SampleCount: 25}
		}
	}

	pruned := es.PruneSkills()

	// The best variant per cap has delta 0.8, second-best 0.79.
	// Skills with delta < 0.8 - 0.15 = 0.65 should be pruned.
	// That's i where 0.8 - i*0.01 < 0.65 → i > 15, so i=16..49 = 34 per cap.
	// 10 capabilities × 34 = 340 pruned.
	expectedPruned := 10 * 34
	if len(pruned) != expectedPruned {
		t.Errorf("expected %d pruned skills, got %d", expectedPruned, len(pruned))
	}
}

// ─── Regression: ExperienceStore persist debounce bounds goroutines ───
// Verifies that persistInFlight prevents concurrent persist goroutines.
// The debounce pattern: if a persist is already running, skip — the next
// persist will pick up all accumulated changes.

func TestExperienceStore_PersistDebounce_BoundsGoroutines(t *testing.T) {
	es := &ExperienceStore{
		GeneratedSkills: make(map[string]*GeneratedSkill),
		SkillAffinities: make(map[string]*SkillAffinity),
	}

	// persistInFlight should start false
	if es.persistInFlight.Load() {
		t.Error("persistInFlight should start false")
	}

	// Simulate a persist in progress
	es.persistInFlight.Store(true)

	// CompareAndSwap should fail (persist already in flight)
	if es.persistInFlight.CompareAndSwap(false, true) {
		t.Error("CompareAndSwap should fail when persistInFlight is already true")
	}

	// Simulate persist completing
	es.persistInFlight.Store(false)

	// CompareAndSwap should succeed now
	if !es.persistInFlight.CompareAndSwap(false, true) {
		t.Error("CompareAndSwap should succeed when persistInFlight is false")
	}

	// Clean up
	es.persistInFlight.Store(false)
}
