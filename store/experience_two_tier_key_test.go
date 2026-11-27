package store

import (
	"testing"
)

// Tests for the two-tier key system: familyKey (coarse, statistical) vs
// stepSequenceKey (fine, JIT candidate judgment).
// Spec: docs/completed/2026-11-27/MEMORY_TWO_TIER_KEY.md §2, §3

func TestFamilyKey_EmptyRecords(t *testing.T) {
	got := familyKey(nil)
	if got != "" {
		t.Fatalf("familyKey(nil) = %q, want %q", got, "")
	}
}

func TestFamilyKey_TemperatureVariant_ToolOrderChange(t *testing.T) {
	// Same capabilities + roles, different tool order → same family
	recsA := []StepRecord{
		{RoleID: "recon", Capability: "security_recon", Trace: []ToolInteraction{{ToolName: "read_file"}, {ToolName: "web_search"}}},
		{RoleID: "scanner", Capability: "vulnerability_scan", Trace: []ToolInteraction{{ToolName: "write_file"}}},
	}
	recsB := []StepRecord{
		{RoleID: "recon", Capability: "security_recon", Trace: []ToolInteraction{{ToolName: "web_search"}, {ToolName: "read_file"}}},
		{RoleID: "scanner", Capability: "vulnerability_scan", Trace: []ToolInteraction{{ToolName: "write_file"}}},
	}
	keyA := familyKey(recsA)
	keyB := familyKey(recsB)
	if keyA != keyB {
		t.Fatalf("tool order change should produce same family key: A=%q, B=%q", keyA, keyB)
	}
}

func TestFamilyKey_TemperatureVariant_StepReordering(t *testing.T) {
	// Same capabilities + roles, different step order → same family
	// (familyKey sorts the capability multiset, so order doesn't matter)
	recsA := []StepRecord{
		{RoleID: "analyst", Capability: "code_analysis"},
		{RoleID: "reviewer", Capability: "code_review"},
		{RoleID: "fixer", Capability: "code_fix"},
	}
	recsB := []StepRecord{
		{RoleID: "fixer", Capability: "code_fix"},
		{RoleID: "analyst", Capability: "code_analysis"},
		{RoleID: "reviewer", Capability: "code_review"},
	}
	keyA := familyKey(recsA)
	keyB := familyKey(recsB)
	if keyA != keyB {
		t.Fatalf("step reordering with same capabilities should produce same family key: A=%q, B=%q", keyA, keyB)
	}
}

func TestFamilyKey_TemperatureVariant_SkillsJitter(t *testing.T) {
	// Different skills, same capabilities + roles → same family
	recsA := []StepRecord{
		{RoleID: "recon", Capability: "security_recon", Skills: []string{"skill_alpha", "skill_beta"}},
	}
	recsB := []StepRecord{
		{RoleID: "recon", Capability: "security_recon", Skills: []string{"skill_gamma"}},
	}
	keyA := familyKey(recsA)
	keyB := familyKey(recsB)
	if keyA != keyB {
		t.Fatalf("skills jitter should produce same family key: A=%q, B=%q", keyA, keyB)
	}
}

func TestFamilyKey_DifferentCapabilities_DifferentFamily(t *testing.T) {
	recsA := []StepRecord{
		{RoleID: "recon", Capability: "security_recon"},
	}
	recsB := []StepRecord{
		{RoleID: "recon", Capability: "data_analysis"},
	}
	keyA := familyKey(recsA)
	keyB := familyKey(recsB)
	if keyA == keyB {
		t.Fatalf("different capabilities should produce different family keys: A=%q, B=%q", keyA, keyB)
	}
}

func TestFamilyKey_DifferentRoles_DifferentFamily(t *testing.T) {
	recsA := []StepRecord{
		{RoleID: "recon", Capability: "security_recon"},
	}
	recsB := []StepRecord{
		{RoleID: "auditor", Capability: "security_recon"},
	}
	keyA := familyKey(recsA)
	keyB := familyKey(recsB)
	if keyA == keyB {
		t.Fatalf("different roles should produce different family keys: A=%q, B=%q", keyA, keyB)
	}
}

func TestFamilyKey_CapabilityMultiset_PreservesDuplicates(t *testing.T) {
	// Same capability appearing multiple times is a multiset, not a set
	recsA := []StepRecord{
		{RoleID: "r1", Capability: "scan"},
		{RoleID: "r1", Capability: "scan"},
		{RoleID: "r1", Capability: "scan"},
	}
	recsB := []StepRecord{
		{RoleID: "r1", Capability: "scan"},
	}
	keyA := familyKey(recsA)
	keyB := familyKey(recsB)
	if keyA == keyB {
		t.Fatalf("capability multiset should distinguish 3×scan from 1×scan: A=%q, B=%q", keyA, keyB)
	}
}

func TestFamilyKeyFromStepSequence_MatchesFamilyKey(t *testing.T) {
	recs := []StepRecord{
		{RoleID: "recon", Capability: "security_recon", Skills: []string{"s1"}},
		{RoleID: "scanner", Capability: "vulnerability_scan"},
	}
	steps := make([]map[string]any, len(recs))
	for i, r := range recs {
		steps[i] = map[string]any{
			"role_id":    r.RoleID,
			"capability": r.Capability,
			"skills":     r.Skills,
		}
	}
	keyFromRecs := familyKey(recs)
	keyFromSteps := familyKeyFromStepSequence(steps)
	if keyFromRecs != keyFromSteps {
		t.Fatalf("familyKey and familyKeyFromStepSequence should match: recs=%q, steps=%q", keyFromRecs, keyFromSteps)
	}
}

func TestFamilyKeyFromStepSequence_Empty(t *testing.T) {
	got := familyKeyFromStepSequence(nil)
	if got != "" {
		t.Fatalf("familyKeyFromStepSequence(nil) = %q, want %q", got, "")
	}
}

func TestUpsertTaskPattern_FamilyKeyStatsAggregation(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	// Two runs with same family but different tool order (different SequenceKey)
	recsA := []StepRecord{
		{RoleID: "recon", Capability: "security_recon", Trace: []ToolInteraction{{ToolName: "read_file"}, {ToolName: "web_search"}}},
	}
	recsB := []StepRecord{
		{RoleID: "recon", Capability: "security_recon", Trace: []ToolInteraction{{ToolName: "web_search"}, {ToolName: "read_file"}}},
	}

	es.upsertTaskPattern("task1", "security_recon", recsA, 0.8, true, false)
	es.upsertTaskPattern("task2", "security_recon", recsB, 0.6, true, false)

	famKey := familyKey(recsA)
	fs, ok := es.familyKeyStats[famKey]
	if !ok {
		t.Fatalf("familyKeyStats should contain key %q", famKey)
	}
	if fs.SampleCount != 2 {
		t.Fatalf("familyKeyStats SampleCount = %d, want 2 (two runs same family)", fs.SampleCount)
	}
	wantAvg := (0.8 + 0.6) / 2
	if fs.AvgConfidence < wantAvg-0.001 || fs.AvgConfidence > wantAvg+0.001 {
		t.Fatalf("familyKeyStats AvgConfidence = %f, want ~%f", fs.AvgConfidence, wantAvg)
	}
}

func TestUpsertTaskPattern_FamilyKeyStats_DifferentFamilies(t *testing.T) {
	es := &ExperienceStore{
		TaskPatterns:     make(map[string]TaskPattern),
		sequenceKeyIndex: make(map[string]string),
		familyKeyStats:   make(map[string]*FamilyStats),
	}

	recsA := []StepRecord{
		{RoleID: "recon", Capability: "security_recon", Trace: []ToolInteraction{{ToolName: "read_file"}}},
	}
	recsB := []StepRecord{
		{RoleID: "analyst", Capability: "data_analysis", Trace: []ToolInteraction{{ToolName: "read_file"}}},
	}

	es.upsertTaskPattern("task1", "security_recon", recsA, 0.8, true, false)
	es.upsertTaskPattern("task2", "data_analysis", recsB, 0.6, true, false)

	keyA := familyKey(recsA)
	keyB := familyKey(recsB)
	if keyA == keyB {
		t.Fatalf("different families should have different keys")
	}
	fsA := es.familyKeyStats[keyA]
	fsB := es.familyKeyStats[keyB]
	if fsA.SampleCount != 1 || fsB.SampleCount != 1 {
		t.Fatalf("different families should each have SampleCount=1, got A=%d B=%d", fsA.SampleCount, fsB.SampleCount)
	}
}
