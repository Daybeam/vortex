package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// Regression for docs/completed/2026-11-05/JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §7.6:
// a JIT tool expires after its TTL while its /skills/<id> config entry and
// GeneratedSkill record persist. Resolution must treat such ids as missing,
// or findskills / affinity recommendations and prompt assembly advertise a
// tool that no longer exists.
func TestGetSkill_FiltersExpiredJITTools(t *testing.T) {
	hub := newTestHub(t, map[string]*config.Skill{
		"jit_live":     {ID: "jit_live", Capability: "live"},
		"jit_dead":     {ID: "jit_dead", Capability: "dead"},
		"jit_promoted": {ID: "jit_promoted", Capability: "promoted"},
		"skill_static": {ID: "skill_static", Capability: "static"},
	}, nil)
	hub.Registry.RegisterDynamicMCP(&config.MCPDef{ID: "jit_live"})
	hub.Registry.MCPs["jit_promoted"] = &config.MCPDef{ID: "jit_promoted"}

	if got := hub.GetSkill("jit_dead"); got != nil {
		t.Fatalf("expired JIT id must not resolve, got %v", got)
	}
	if got := hub.GetSkill("jit_live"); got == nil {
		t.Fatal("JIT tool still in DynamicMCPs must resolve")
	}
	if got := hub.GetSkill("jit_promoted"); got == nil {
		t.Fatal("promoted JIT tool (Registry.MCPs) must resolve")
	}
	if got := hub.GetSkill("skill_static"); got == nil {
		t.Fatal("non-JIT skills must resolve normally")
	}
}

// Same dead-reference rule for capability matching: a dead JIT entry must not
// win a FindSkillByCapability match and get mounted after validation.
func TestFindSkillByCapability_SkipsExpiredJITTools(t *testing.T) {
	hub := newTestHub(t, map[string]*config.Skill{
		"jit_dead": {ID: "jit_dead", Capability: "fetch market data"},
	}, nil)

	if got := hub.FindSkillByCapability("fetch market data"); got != nil {
		t.Fatalf("expired JIT skill must not match, got %v", got)
	}

	hub.Registry.RegisterDynamicMCP(&config.MCPDef{ID: "jit_dead"})
	if got := hub.FindSkillByCapability("fetch market data"); got == nil || got.ID != "jit_dead" {
		t.Fatalf("live JIT skill should match, got %v", got)
	}
}
