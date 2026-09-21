package store

import (
	"context"
	"testing"
)

// TestPruneSkills_ArchivesLowPerformingVariant_NotDeleted is a regression
// test for the 2026-08-01 PruneSkills implementation: a low-performing
// variant (ParentID set, enough samples, meaningfully worse than its best
// credibly-sampled sibling) should be archived, not simply deleted.
func TestPruneSkills_ArchivesLowPerformingVariant_NotDeleted(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["root_a"] = &GeneratedSkill{ID: "root_a", Capability: "cap_x", ParentID: ""}
	es.GeneratedSkills["variant_a"] = &GeneratedSkill{ID: "variant_a", Capability: "cap_x", ParentID: "root_a"}
	es.SkillAffinities["cap_x:root_a"] = &SkillAffinity{BaseCapability: "cap_x", AddedSkill: "root_a", ConfidenceDelta: 0.8, SampleCount: 25}
	es.SkillAffinities["cap_x:variant_a"] = &SkillAffinity{BaseCapability: "cap_x", AddedSkill: "variant_a", ConfidenceDelta: 0.3, SampleCount: 25}

	pruned := es.PruneSkills()

	if len(pruned) != 1 || pruned[0] != "variant_a" {
		t.Fatalf("expected [variant_a] pruned, got %v", pruned)
	}
	if _, stillThere := es.GeneratedSkills["variant_a"]; stillThere {
		t.Fatalf("variant_a should have been removed from GeneratedSkills")
	}
	archived, ok := es.ArchivedSkills["variant_a"]
	if !ok {
		t.Fatalf("variant_a should be present in ArchivedSkills, not just deleted")
	}
	if archived.Capability != "cap_x" {
		t.Fatalf("archived record lost its data: %+v", archived)
	}
	if _, rootStillThere := es.GeneratedSkills["root_a"]; !rootStillThere {
		t.Fatalf("root_a should never be touched")
	}
}

// TestPruneSkills_NeverPrunesRoot covers the root-protection guarantee: a
// root (ParentID == "") must never be pruned regardless of how badly it
// scores relative to its variants.
func TestPruneSkills_NeverPrunesRoot(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["root_a"] = &GeneratedSkill{ID: "root_a", Capability: "cap_x", ParentID: ""}
	es.GeneratedSkills["variant_a"] = &GeneratedSkill{ID: "variant_a", Capability: "cap_x", ParentID: "root_a"}
	es.SkillAffinities["cap_x:root_a"] = &SkillAffinity{ConfidenceDelta: 0.1, SampleCount: 50}
	es.SkillAffinities["cap_x:variant_a"] = &SkillAffinity{ConfidenceDelta: 0.9, SampleCount: 50}

	pruned := es.PruneSkills()

	for _, id := range pruned {
		if id == "root_a" {
			t.Fatalf("root_a must never be pruned, regardless of score")
		}
	}
	if _, ok := es.GeneratedSkills["root_a"]; !ok {
		t.Fatalf("root_a should remain in GeneratedSkills")
	}
}

// TestPruneSkills_SkipsUnderSampledVariant covers the minSamplesToPrune gate.
func TestPruneSkills_SkipsUnderSampledVariant(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["root_a"] = &GeneratedSkill{ID: "root_a", Capability: "cap_x", ParentID: ""}
	es.GeneratedSkills["variant_a"] = &GeneratedSkill{ID: "variant_a", Capability: "cap_x", ParentID: "root_a"}
	es.SkillAffinities["cap_x:root_a"] = &SkillAffinity{ConfidenceDelta: 0.9, SampleCount: 50}
	es.SkillAffinities["cap_x:variant_a"] = &SkillAffinity{ConfidenceDelta: 0.1, SampleCount: 5}

	pruned := es.PruneSkills()

	if len(pruned) != 0 {
		t.Fatalf("expected no pruning for an under-sampled variant, got %v", pruned)
	}
}

// TestPruneSkills_SkipsWhenNoCredibleSibling covers the case where a
// variant has enough samples but no sibling does yet, so there is no fair
// baseline to judge it against.
func TestPruneSkills_SkipsWhenNoCredibleSibling(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["root_a"] = &GeneratedSkill{ID: "root_a", Capability: "cap_x", ParentID: ""}
	es.GeneratedSkills["variant_a"] = &GeneratedSkill{ID: "variant_a", Capability: "cap_x", ParentID: "root_a"}
	es.SkillAffinities["cap_x:variant_a"] = &SkillAffinity{ConfidenceDelta: 0.1, SampleCount: 30}

	pruned := es.PruneSkills()

	if len(pruned) != 0 {
		t.Fatalf("expected no pruning without a credibly-sampled sibling to compare against, got %v", pruned)
	}
}

// TestRootSkillForCapability_ReturnsExistingRootOrEmpty covers the lineage
// lookup that reflection.go.registerAsSkill now uses to decide ParentID for
// a newly-generated skill.
func TestRootSkillForCapability_ReturnsExistingRootOrEmpty(t *testing.T) {
	es := newTestStore(t)

	if got := es.RootSkillForCapability("cap_new"); got != "" {
		t.Fatalf("expected empty string for a capability with no skills yet, got %q", got)
	}

	es.GeneratedSkills["root_a"] = &GeneratedSkill{ID: "root_a", Capability: "cap_x", ParentID: ""}
	es.GeneratedSkills["variant_a"] = &GeneratedSkill{ID: "variant_a", Capability: "cap_x", ParentID: "root_a"}

	if got := es.RootSkillForCapability("cap_x"); got != "root_a" {
		t.Fatalf("expected root_a, got %q", got)
	}
}

// TestPersistAll_ArchivedSkillsRoundTrips is a direct regression test for
// the fact that ArchivedSkills was previously declared and initialized but
// never wired into load()/PersistAll() at all -- a completely dead map.
func TestPersistAll_ArchivedSkillsRoundTrips(t *testing.T) {
	es := newTestStore(t)
	es.ArchivedSkills["variant_a"] = &GeneratedSkill{ID: "variant_a", Capability: "cap_x", ParentID: "root_a", Description: "an archived variant"}

	if err := es.PersistAll(context.Background()); err != nil {
		t.Fatalf("PersistAll: %v", err)
	}

	es2, err := NewExperienceStore(es.dir, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewExperienceStore reload: %v", err)
	}
	got, ok := es2.ArchivedSkills["variant_a"]
	if !ok {
		t.Fatalf("expected variant_a to round-trip through persistence")
	}
	if got.Description != "an archived variant" {
		t.Fatalf("round-tripped record lost data: %+v", got)
	}
}
