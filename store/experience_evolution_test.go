package store

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestExperienceStore_EnvironmentFingerprinting(t *testing.T) {
	dir := t.TempDir()
	es, _ := NewExperienceStore(dir, nil, nil, nil, nil)

	// 1. Add a skill for a DIFFERENT OS
	otherOS := "linux"
	if runtime.GOOS == "linux" {
		otherOS = "windows"
	}

	es.AddGeneratedSkill(context.Background(), &GeneratedSkill{
		ID:          "skill_other_os",
		Description: "A skill for another OS",
		Capability:  "test",
		OS:          otherOS,
	})

	// 2. Add a skill for the CURRENT OS
	es.AddGeneratedSkill(context.Background(), &GeneratedSkill{
		ID:          "skill_current_os",
		Description: "A skill for current OS",
		Capability:  "test",
		OS:          runtime.GOOS,
	})

	// 3. Verify retrieval
	relevant := es.QueryRelevantSkills(context.Background(), "test", 10)

	foundCurrent := false
	foundOther := false
	for _, id := range relevant {
		if id == "skill_current_os" {
			foundCurrent = true
		}
		if id == "skill_other_os" {
			foundOther = true
		}
	}

	if !foundCurrent {
		t.Error("Expected to find skill for current OS")
	}
	if foundOther {
		t.Error("Did NOT expect to find skill for other OS")
	}
}

func TestExperienceStore_TemporalDecay(t *testing.T) {
	dir := t.TempDir()
	es, _ := NewExperienceStore(dir, nil, nil, nil, nil)

	// Add an old skill
	oldID := "skill_old"
	es.AddGeneratedSkill(context.Background(), &GeneratedSkill{
		ID:          oldID,
		Capability:  "test",
		SuccessRate: 1.0,
		UsageCount:  10,
		LastUsed:    time.Now().AddDate(0, -2, 0), // 2 months ago
	})

	// Add a fresh skill
	freshID := "skill_fresh"
	es.AddGeneratedSkill(context.Background(), &GeneratedSkill{
		ID:          freshID,
		Description: "Fresh",
		Capability:  "test",
		UsageCount:  1,
		SuccessRate: 1.0,
		LastUsed:    time.Now(),
	})

	// Force decay apply
	es.applyTemporalDecayLocked()

	gs, _ := es.GetGeneratedSkill(oldID)
	if gs.SuccessRate >= 1.0 {
		t.Errorf("Expected SuccessRate to decay, got %f", gs.SuccessRate)
	}
	if gs.UsageCount >= 10 {
		t.Errorf("Expected UsageCount to decay, got %d", gs.UsageCount)
	}

	freshGs, _ := es.GetGeneratedSkill(freshID)
	if freshGs.SuccessRate < 1.0 {
		t.Error("Did NOT expect fresh skill to decay")
	}
}

func TestExperienceStore_SignalDrivenRetrieval(t *testing.T) {
	dir := t.TempDir()
	es, _ := NewExperienceStore(dir, nil, nil, nil, nil)

	signal := "Access Denied: Permission Error"
	es.AddGeneratedSkill(context.Background(), &GeneratedSkill{
		ID:            "skill_fix_auth",
		Capability:    "auth",
		FailureSignal: signal,
		OS:            runtime.GOOS,
	})

	// Query by signal
	repairs := es.QuerySkillsBySignal(context.Background(), "Something went wrong, Access Denied: Permission Error on step 1")

	if len(repairs) == 0 || repairs[0] != "skill_fix_auth" {
		t.Errorf("Expected to find repair skill by signal, got %v", repairs)
	}

	// Query by unrelated signal
	none := es.QuerySkillsBySignal(context.Background(), "Unknown error")
	if len(none) > 0 {
		t.Errorf("Expected no repair skills for unrelated signal, got %v", none)
	}
}
