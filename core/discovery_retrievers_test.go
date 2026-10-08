package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func makeDiscoveryTestRegistry() *config.Registry {
	return &config.Registry{
		Skills: make(map[string]*config.Skill),
		Roles:  make(map[string]*config.Role),
		MCPs:   make(map[string]*config.MCPDef),
		SOPs:   make(map[string]*schemas.SOP),
	}
}

// --- KeywordRetriever ---

func TestKeywordRetriever_MatchesSkillsByName(t *testing.T) {
	reg := makeDiscoveryTestRegistry()
	reg.Skills["ext_threejs"] = &config.Skill{
		ID:          "ext_threejs",
		Name:        "ThreeJS Game Director",
		Description: "Directs threejs game development",
	}

	r := &KeywordRetriever{Registry: reg}
	candidates := r.Recall(context.Background(), "threejs", nil)

	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Type != CandidateSkill {
		t.Errorf("expected CandidateSkill, got %v", candidates[0].Type)
	}
	if candidates[0].ID != "ext_threejs" {
		t.Errorf("expected ext_threejs, got %s", candidates[0].ID)
	}
	if candidates[0].Source != "keyword" {
		t.Errorf("expected source=keyword, got %s", candidates[0].Source)
	}
}

func TestKeywordRetriever_MatchesSkillsByDescription(t *testing.T) {
	reg := makeDiscoveryTestRegistry()
	reg.Skills["ext_qa"] = &config.Skill{
		ID:          "ext_qa",
		Name:        "QA Skill",
		Description: "Automated quality assurance testing",
	}

	r := &KeywordRetriever{Registry: reg}
	candidates := r.Recall(context.Background(), "quality assurance", nil)

	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].ID != "ext_qa" {
		t.Errorf("expected ext_qa, got %s", candidates[0].ID)
	}
}

func TestKeywordRetriever_NoMatch(t *testing.T) {
	reg := makeDiscoveryTestRegistry()
	reg.Skills["ext_x"] = &config.Skill{
		ID:          "ext_x",
		Name:        "X",
		Description: "Something unrelated",
	}

	r := &KeywordRetriever{Registry: reg}
	candidates := r.Recall(context.Background(), "threejs", nil)

	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates, got %d", len(candidates))
	}
}

// --- FilterRetriever ---

func TestFilterRetriever_MatchesSkillsByProvides(t *testing.T) {
	reg := makeDiscoveryTestRegistry()
	reg.Skills["ext_game"] = &config.Skill{
		ID:       "ext_game",
		Name:     "Game Director",
		Provides: []string{"game", "threejs", "director"},
	}

	idx := NewCapabilityIndex(reg)
	r := &FilterRetriever{Index: idx, Registry: reg}
	candidates := r.Recall(context.Background(), "", []string{"threejs"})

	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Type != CandidateSkill {
		t.Errorf("expected CandidateSkill, got %v", candidates[0].Type)
	}
	if candidates[0].Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", candidates[0].Confidence)
	}
}

// --- SemanticRetriever ---

func TestSemanticRetriever_MatchesSkillsByProvides(t *testing.T) {
	reg := makeDiscoveryTestRegistry()
	reg.Skills["ext_3d"] = &config.Skill{
		ID:       "ext_3d",
		Name:     "3D Director",
		Provides: []string{"threejs", "3d", "rendering"},
	}

	graph := NewCapabilityGraph()
	r := &SemanticRetriever{Graph: graph, Registry: reg}
	// With a tag that matches Provides directly (graph returns empty for unknown tags,
	// but the tag itself is in the expanded set if GetRelated returns it)
	candidates := r.Recall(context.Background(), "threejs", []string{"threejs"})

	found := false
	for _, c := range candidates {
		if c.Type == CandidateSkill && c.ID == "ext_3d" {
			found = true
			if c.Source != "semantic" {
				t.Errorf("expected source=semantic, got %s", c.Source)
			}
		}
	}
	if !found {
		t.Error("expected to find ext_3d skill via Provides match")
	}
}

func TestSemanticRetriever_LegacyCapabilityFallback(t *testing.T) {
	reg := makeDiscoveryTestRegistry()
	reg.Skills["ext_legacy"] = &config.Skill{
		ID:         "ext_legacy",
		Name:       "Legacy Skill",
		Capability: "external_discipline", // single-value, no Provides
	}

	graph := NewCapabilityGraph()
	r := &SemanticRetriever{Graph: graph, Registry: reg}
	candidates := r.Recall(context.Background(), "test", []string{"external_discipline"})

	found := false
	for _, c := range candidates {
		if c.Type == CandidateSkill && c.ID == "ext_legacy" {
			found = true
		}
	}
	if !found {
		t.Error("expected to find ext_legacy skill via legacy Capability fallback")
	}
}

func TestSemanticRetriever_ProvidesTakesPrecedenceOverCapability(t *testing.T) {
	reg := makeDiscoveryTestRegistry()
	reg.Skills["ext_both"] = &config.Skill{
		ID:         "ext_both",
		Name:       "Both Fields",
		Capability: "old_capability",
		Provides:   []string{"new_capability"},
	}

	graph := NewCapabilityGraph()
	r := &SemanticRetriever{Graph: graph, Registry: reg}
	// Should match via Provides, not via legacy Capability
	candidates := r.Recall(context.Background(), "test", []string{"new_capability", "old_capability"})

	matchCount := 0
	for _, c := range candidates {
		if c.Type == CandidateSkill && c.ID == "ext_both" {
			matchCount++
		}
	}
	// Should match exactly once (not twice) — Provides takes precedence,
	// legacy fallback only triggers when Provides is empty
	if matchCount != 1 {
		t.Errorf("expected exactly 1 match (Provides precedence), got %d", matchCount)
	}
}
