package store

import (
	"testing"
	"time"
)

func TestUpdateSkillMetabolicCost_ROICalculation(t *testing.T) {
	es := newTestStore(t)

	es.Mu.Lock()
	es.GeneratedSkills["skill_a"] = &GeneratedSkill{
		ID:          "skill_a",
		Capability:  "coding",
		SuccessRate: 1.0,
		SkillStatus: "active",
	}
	es.Mu.Unlock()

	rec := StepRecord{
		Skills:     []string{"skill_a"},
		Status:     "ok",
		RetryCount: 0,
		TokenUsed:  5000,
		LatencyMs:  2000,
	}

	es.Mu.Lock()
	es.updateSkillMetabolicCostLocked(&rec)
	es.Mu.Unlock()

	gs := es.GeneratedSkills["skill_a"]
	if gs.TokenCostTotal != 5000 {
		t.Errorf("TokenCostTotal = %d, want 5000", gs.TokenCostTotal)
	}
	if gs.UsageCount != 1 {
		t.Errorf("UsageCount = %d, want 1", gs.UsageCount)
	}
	if gs.AvgLatencyMs != 2000 {
		t.Errorf("AvgLatencyMs = %f, want 2000", gs.AvgLatencyMs)
	}

	cost := 0.3*float64(gs.TokenCostTotal)/1000.0 + 0.3*gs.AvgLatencyMs/1000.0 + 0.4*0
	expectedROI := (1.0 * 100.0) / cost
	if gs.MetabolicROI < expectedROI-0.01 || gs.MetabolicROI > expectedROI+0.01 {
		t.Errorf("MetabolicROI = %f, want ~%f", gs.MetabolicROI, expectedROI)
	}
}

func TestUpdateSkillMetabolicCost_FailureLowersSuccessRate(t *testing.T) {
	es := newTestStore(t)

	es.Mu.Lock()
	es.GeneratedSkills["skill_b"] = &GeneratedSkill{
		ID:          "skill_b",
		Capability:  "coding",
		SuccessRate: 1.0,
		UsageCount:  4,
		SkillStatus: "active",
	}
	es.Mu.Unlock()

	rec := StepRecord{
		Skills:     []string{"skill_b"},
		Status:     "failed",
		RetryCount: 3,
		TokenUsed:  10000,
	}

	es.Mu.Lock()
	es.updateSkillMetabolicCostLocked(&rec)
	es.Mu.Unlock()

	gs := es.GeneratedSkills["skill_b"]
	if gs.SuccessRate > 0.85 {
		t.Errorf("SuccessRate = %f, should be < 0.85 after failure", gs.SuccessRate)
	}
	if gs.MetabolicROI < 0.2 {
		t.Logf("ROI = %f (low ROI as expected for high-cost failure)", gs.MetabolicROI)
	}
}

func TestPruneByMetabolicROI_MarksDormant(t *testing.T) {
	es := newTestStore(t)

	es.Mu.Lock()
	es.GeneratedSkills["low_roi_variant"] = &GeneratedSkill{
		ID:           "low_roi_variant",
		Capability:   "coding",
		ParentID:     "root_skill",
		MetabolicROI: 0.1,
		SkillStatus:  "active",
		LastUsed:     time.Now(),
	}
	es.GeneratedSkills["healthy_variant"] = &GeneratedSkill{
		ID:           "healthy_variant",
		Capability:   "coding",
		ParentID:     "root_skill",
		MetabolicROI: 5.0,
		SkillStatus:  "active",
		LastUsed:     time.Now(),
	}
	es.Mu.Unlock()

	pruned := es.PruneByMetabolicROI()
	if len(pruned) != 0 {
		t.Errorf("expected 0 pruned, got %d", len(pruned))
	}

	gs := es.GeneratedSkills["low_roi_variant"]
	if gs.SkillStatus != "dormant" {
		t.Errorf("low ROI skill status = %q, want 'dormant'", gs.SkillStatus)
	}
	gs2 := es.GeneratedSkills["healthy_variant"]
	if gs2.SkillStatus != "active" {
		t.Errorf("healthy skill status = %q, want 'active'", gs2.SkillStatus)
	}
}

func TestPruneByMetabolicROI_PhysicallyRemovesOldDormant(t *testing.T) {
	es := newTestStore(t)

	es.Mu.Lock()
	es.GeneratedSkills["old_dormant"] = &GeneratedSkill{
		ID:           "old_dormant",
		Capability:   "coding",
		ParentID:     "root_skill",
		MetabolicROI: 0.1,
		SkillStatus:  "dormant",
		LastUsed:     time.Now().Add(-61 * 24 * time.Hour),
	}
	es.Mu.Unlock()

	pruned := es.PruneByMetabolicROI()
	if len(pruned) != 1 || pruned[0] != "old_dormant" {
		t.Errorf("expected [old_dormant], got %v", pruned)
	}
	if _, exists := es.GeneratedSkills["old_dormant"]; exists {
		t.Error("old_dormant should be deleted from GeneratedSkills")
	}
	if _, exists := es.ArchivedSkills["old_dormant"]; !exists {
		t.Error("old_dormant should be in ArchivedSkills")
	}
}

func TestPruneByMetabolicROI_NeverPrunesRoot(t *testing.T) {
	es := newTestStore(t)

	es.Mu.Lock()
	es.GeneratedSkills["root_skill"] = &GeneratedSkill{
		ID:           "root_skill",
		Capability:   "coding",
		ParentID:     "",
		MetabolicROI: 0.05,
		SkillStatus:  "active",
		LastUsed:     time.Now().Add(-90 * 24 * time.Hour),
	}
	es.Mu.Unlock()

	pruned := es.PruneByMetabolicROI()
	if len(pruned) != 0 {
		t.Errorf("root skill should never be pruned, got %v", pruned)
	}
	gs := es.GeneratedSkills["root_skill"]
	if gs.SkillStatus != "active" {
		t.Errorf("root skill status = %q, should remain 'active'", gs.SkillStatus)
	}
}
