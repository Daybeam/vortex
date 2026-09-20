package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func newTestHub(t *testing.T, globalSkills map[string]*config.Skill, sessionSkills map[string]any) *ContextHub {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "hub_capability_test")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	reg, err := config.NewRegistry(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for id, s := range globalSkills {
		reg.Skills[id] = s
	}

	graph := &schemas.TaskGraph{
		TaskID:        "task_test",
		SessionSkills: sessionSkills,
	}
	return NewContextHub(reg, graph, nil)
}

func TestFindSkillByCapability_ExactIDMatch(t *testing.T) {
	hub := newTestHub(t, map[string]*config.Skill{
		"skill_fetch_market_data": {ID: "skill_fetch_market_data", Capability: "market data retrieval"},
	}, nil)

	got := hub.FindSkillByCapability("skill_fetch_market_data")
	if got == nil || got.ID != "skill_fetch_market_data" {
		t.Fatalf("expected exact ID match, got %v", got)
	}
}

func TestFindSkillByCapability_ExactCapabilityMatch(t *testing.T) {
	hub := newTestHub(t, map[string]*config.Skill{
		"skill_fetch_market_data": {ID: "skill_fetch_market_data", Capability: "fetch market data"},
	}, nil)

	got := hub.FindSkillByCapability("fetch market data")
	if got == nil || got.ID != "skill_fetch_market_data" {
		t.Fatalf("expected exact Capability match, got %v", got)
	}
}

// TestFindSkillByCapability_TokenOverlapMatch is the actual motivating case:
// the LLM says something like "fetch market data" and the registered skill's
// snake_case ID is skill_fetch_market_data with a differently-worded
// Capability string -- must still match via whole-token overlap, not an
// exact string match.
func TestFindSkillByCapability_TokenOverlapMatch(t *testing.T) {
	hub := newTestHub(t, map[string]*config.Skill{
		"skill_fetch_market_data": {ID: "skill_fetch_market_data", Capability: "Pulls quote and macro data from exa/fred/tushare"},
	}, nil)

	got := hub.FindSkillByCapability("fetch market data")
	if got == nil || got.ID != "skill_fetch_market_data" {
		t.Fatalf("expected token-overlap match via ID tokens, got %v", got)
	}
}

func TestFindSkillByCapability_NoMatchReturnsNil(t *testing.T) {
	hub := newTestHub(t, map[string]*config.Skill{
		"skill_fetch_market_data": {ID: "skill_fetch_market_data", Capability: "fetch market data"},
	}, nil)

	got := hub.FindSkillByCapability("render a video thumbnail")
	if got != nil {
		t.Fatalf("expected no match, got %v", got)
	}
}

// TestFindSkillByCapability_StopwordOnlyDoesNotMatch mirrors the exact
// failure mode caught in store/experience.go's tokenizeStopwords work
// (2026-07-10 addendum): a description consisting only of common words
// must not spuriously match every skill.
func TestFindSkillByCapability_StopwordOnlyDoesNotMatch(t *testing.T) {
	hub := newTestHub(t, map[string]*config.Skill{
		"skill_fetch_market_data": {ID: "skill_fetch_market_data", Capability: "fetch market data"},
	}, nil)

	got := hub.FindSkillByCapability("please do the thing for me")
	if got != nil {
		t.Fatalf("expected stopword-only description to not match, got %v", got)
	}
}

// TestFindSkillByCapability_DeterministicNoCrossMatch is the key regression
// test for the bug this function replaced. Two skills deliberately share one
// non-discriminating token ("data") but are otherwise unrelated; the
// description overlaps heavily with only one of them. Run the lookup many
// times (Go randomizes map iteration order per range statement, so a
// map-order-dependent implementation would eventually show a different
// answer across repeated calls within the same process) and assert every
// single call returns the same, correct skill.
func TestFindSkillByCapability_DeterministicNoCrossMatch(t *testing.T) {
	skills := map[string]*config.Skill{
		"skill_fetch_market_data":  {ID: "skill_fetch_market_data", Capability: "fetch market quote data"},
		"skill_fetch_weather_data": {ID: "skill_fetch_weather_data", Capability: "fetch weather forecast data"},
		"skill_render_report":      {ID: "skill_render_report", Capability: "render a formatted report document"},
	}
	hub := newTestHub(t, skills, nil)

	for i := 0; i < 200; i++ {
		got := hub.FindSkillByCapability("fetch market quote data")
		if got == nil {
			t.Fatalf("iteration %d: expected a match, got nil", i)
		}
		if got.ID != "skill_fetch_market_data" {
			t.Fatalf("iteration %d: cross-matched wrong skill %q (expected skill_fetch_market_data) -- non-deterministic map-order bug reproduced", i, got.ID)
		}
	}
}

func TestFindSkillByCapability_SessionSkillTakesPriorityOverGlobal(t *testing.T) {
	globalSkill := &config.Skill{ID: "skill_fetch_market_data", Capability: "fetch market data (global, stale)"}
	sessionSkill := &config.Skill{ID: "skill_fetch_market_data_v2", Capability: "fetch market data (session, ephemeral)"}

	hub := newTestHub(t,
		map[string]*config.Skill{"skill_fetch_market_data": globalSkill},
		map[string]any{"skill_fetch_market_data_v2": sessionSkill},
	)

	got := hub.FindSkillByCapability("fetch market data")
	if got == nil || got.ID != "skill_fetch_market_data_v2" {
		t.Fatalf("expected session skill to win over global, got %v", got)
	}
}

func TestFindSkillByCapability_EmptyDescriptionReturnsNil(t *testing.T) {
	hub := newTestHub(t, map[string]*config.Skill{
		"skill_fetch_market_data": {ID: "skill_fetch_market_data", Capability: "fetch market data"},
	}, nil)

	if got := hub.FindSkillByCapability(""); got != nil {
		t.Fatalf("expected nil for empty description, got %v", got)
	}
}
