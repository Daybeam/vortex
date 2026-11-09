package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ─── Integration: DiscoverySyncer → multi-source SyncAll ────────────────────

// TestIntegration_MultiSourceSyncAll verifies that a single SyncAll run with
// multiple sources of different kinds and trust levels correctly populates
// all three registries: Skills (trusted), PendingSkills (untrusted), and
// DynamicMCPs (always admin_only).
func TestIntegration_MultiSourceSyncAll(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			// Trusted local skill source → Skills
			&mockDiscoverySource{
				name: "local-skills",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "local-1", Name: "Local 1", TrustLevel: TrustLevelTrusted},
					{Kind: DiscoveryKindSkill, ID: "local-2", Name: "Local 2", TrustLevel: TrustLevelTrusted},
				},
			},
			// Untrusted remote skill source → PendingSkills
			&mockDiscoverySource{
				name: "remote-skills",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "remote-1", Name: "Remote 1", TrustLevel: TrustLevelUntrusted},
				},
			},
			// MCP discovery source → DynamicMCPs (admin_only)
			&mockDiscoverySource{
				name: "glama-mcp",
				kind: DiscoveryKindMCP,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindMCP, ID: "mcp-1", Name: "MCP 1", URL: "https://example.com/mcp1"},
					{Kind: DiscoveryKindMCP, ID: "mcp-2", Name: "MCP 2", URL: "https://example.com/mcp2"},
				},
			},
		},
	}

	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	// Trusted skills → Skills
	if len(reg.Skills) != 2 {
		t.Fatalf("expected 2 trusted skills in Skills, got %d", len(reg.Skills))
	}
	if reg.Skills["local-1"] == nil {
		t.Fatal("local-1 not in Skills")
	}
	if reg.Skills["local-2"] == nil {
		t.Fatal("local-2 not in Skills")
	}

	// Untrusted skills → PendingSkills
	if len(reg.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill, got %d", len(reg.PendingSkills))
	}
	if reg.PendingSkills["remote-1"] == nil {
		t.Fatal("remote-1 not in PendingSkills")
	}

	// MCPs → DynamicMCPs with admin_only
	if len(reg.DynamicMCPs) != 2 {
		t.Fatalf("expected 2 DynamicMCPs, got %d", len(reg.DynamicMCPs))
	}
	for _, mcp := range reg.DynamicMCPs {
		if mcp.MountPolicy != config.MountPolicyAdminOnly {
			t.Fatalf("expected admin_only, got %q", mcp.MountPolicy)
		}
		if mcp.Source != config.SourceExternalDynamic {
			t.Fatalf("expected external_dynamic, got %q", mcp.Source)
		}
	}
}

// ─── Integration: DiscoverySyncer → OnUnknownMCP resolution ────────────────

// TestIntegration_DiscoveryToOnUnknownMCP verifies the full flow:
// 1. DiscoverySyncer discovers MCPs from a source
// 2. MCPs are ingested into DynamicMCPs with admin_only
// 3. NewContextHub's OnUnknownMCP callback resolves the discovered MCP
// 4. The resolved MCP carries the correct security attributes
func TestIntegration_DiscoveryToOnUnknownMCP(t *testing.T) {
	reg := &config.Registry{
		Providers:   make(map[string]*config.ProviderConfig),
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
		Skills:      make(map[string]*config.Skill),
	}

	// 1. DiscoverySyncer discovers and ingests MCPs
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "glama",
				kind: DiscoveryKindMCP,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindMCP, ID: "discovered-mcp", Name: "Discovered MCP", URL: "https://glama.ai/mcp"},
				},
			},
		},
	}
	d.SyncAll()

	// 2. Verify DynamicMCPs populated
	reg.Mu.RLock()
	if len(reg.DynamicMCPs) != 1 {
		t.Fatalf("expected 1 DynamicMCP, got %d", len(reg.DynamicMCPs))
	}
	reg.Mu.RUnlock()

	// 3. Create ContextHub — OnUnknownMCP is auto-wired by NewContextHub
	graph := &schemas.TaskGraph{}
	hub := NewContextHub(reg, graph, nil)

	if hub.OnUnknownMCP == nil {
		t.Fatal("OnUnknownMCP should be wired by NewContextHub")
	}

	// 4. Resolve the discovered MCP via OnUnknownMCP
	result := hub.OnUnknownMCP("discovered-mcp")
	if result == nil {
		t.Fatal("OnUnknownMCP returned nil for a discovered MCP")
	}

	// 5. Verify security attributes
	if result.MountPolicy != config.MountPolicyAdminOnly {
		t.Fatalf("expected admin_only, got %q", result.MountPolicy)
	}
	if result.Source != config.SourceExternalDynamic {
		t.Fatalf("expected external_dynamic, got %q", result.Source)
	}
	if result.URL != "https://glama.ai/mcp" {
		t.Fatalf("expected URL https://glama.ai/mcp, got %q", result.URL)
	}

	// 6. Verify unknown MCP returns nil
	if hub.OnUnknownMCP("nonexistent") != nil {
		t.Fatal("OnUnknownMCP should return nil for unknown MCP")
	}
}

// ─── Integration: Discovery → PendingSkills → promote ──────────────────────

// TestIntegration_DiscoveryToPendingToPromote verifies the full flow:
// 1. DiscoverySyncer discovers skills from an untrusted source
// 2. Skills go to PendingSkills (not directly to Skills)
// 3. Simulate promote_skill: move from PendingSkills to Skills
// 4. Verify the promoted skill is now in Skills and removed from PendingSkills
func TestIntegration_DiscoveryToPendingToPromote(t *testing.T) {
	reg := newDiscoveryTestRegistry()

	// 1. DiscoverySyncer discovers untrusted skills
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "remote-skills",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "remote-skill-1", Name: "Remote Skill 1", TrustLevel: TrustLevelUntrusted},
				},
			},
		},
	}
	d.SyncAll()

	// 2. Verify skill is in PendingSkills, NOT in Skills
	reg.Mu.RLock()
	if len(reg.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill, got %d", len(reg.PendingSkills))
	}
	if len(reg.Skills) != 0 {
		t.Fatalf("expected 0 skills (untrusted should not be in Skills), got %d", len(reg.Skills))
	}
	pendingSkill := reg.PendingSkills["remote-skill-1"]
	reg.Mu.RUnlock()

	if pendingSkill == nil {
		t.Fatal("remote-skill-1 not found in PendingSkills")
	}

	// 3. Simulate promote_skill action (same logic as tools/subsystems_admin.go)
	reg.Mu.Lock()
	reg.Skills["remote-skill-1"] = pendingSkill
	delete(reg.PendingSkills, "remote-skill-1")
	reg.Mu.Unlock()

	// 4. Verify promoted skill is in Skills, not PendingSkills
	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if len(reg.Skills) != 1 {
		t.Fatalf("expected 1 skill after promote, got %d", len(reg.Skills))
	}
	if reg.Skills["remote-skill-1"] == nil {
		t.Fatal("promoted skill not found in Skills")
	}
	if len(reg.PendingSkills) != 0 {
		t.Fatalf("expected 0 pending skills after promote, got %d", len(reg.PendingSkills))
	}
}

// ─── Integration: Discovery → PendingSkills → GC removal ───────────────────

// TestIntegration_DiscoveryToPendingToGC verifies the full flow:
// 1. DiscoverySyncer discovers skills from an untrusted source
// 2. Skills go to PendingSkills with StagedAt timestamp
// 3. Age the StagedAt past the TTL
// 4. Run PendingSkillGC sweep → expired skills are removed
func TestIntegration_DiscoveryToPendingToGC(t *testing.T) {
	reg := newDiscoveryTestRegistry()

	// 1. DiscoverySyncer discovers untrusted skills
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "remote-skills",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "expired-skill", Name: "Expired", TrustLevel: TrustLevelUntrusted},
					{Kind: DiscoveryKindSkill, ID: "fresh-skill", Name: "Fresh", TrustLevel: TrustLevelUntrusted},
				},
			},
		},
	}
	d.SyncAll()

	// 2. Verify both in PendingSkills
	reg.Mu.RLock()
	if len(reg.PendingSkills) != 2 {
		t.Fatalf("expected 2 pending skills, got %d", len(reg.PendingSkills))
	}
	reg.Mu.RUnlock()

	// 3. Age one skill past TTL (default 168h)
	reg.Mu.Lock()
	reg.PendingSkills["expired-skill"].StagedAt = time.Now().Add(-200 * time.Hour)
	reg.Mu.Unlock()

	// 4. Run GC sweep
	gc := NewPendingSkillGC(reg)
	removed := gc.SweepOnce()

	if removed != 1 {
		t.Fatalf("expected 1 removed by GC, got %d", removed)
	}

	// 5. Verify expired skill removed, fresh skill remains
	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if _, ok := reg.PendingSkills["expired-skill"]; ok {
		t.Fatal("expired skill should have been removed by GC")
	}
	if _, ok := reg.PendingSkills["fresh-skill"]; !ok {
		t.Fatal("fresh skill should still be in PendingSkills")
	}
}

// ─── Integration: Discovery → trusted + untrusted skill coexistence ─────────

// TestIntegration_DiscoveryTrustedAndUntrustedCoexist verifies that trusted
// and untrusted skills discovered in the same SyncAll run coexist correctly:
// trusted skills go to Skills, untrusted go to PendingSkills, and neither
// interferes with the other.
func TestIntegration_DiscoveryTrustedAndUntrustedCoexist(t *testing.T) {
	reg := newDiscoveryTestRegistry()

	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			// Trusted source
			&mockDiscoverySource{
				name: "local-dir",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "trusted-a", Name: "Trusted A", TrustLevel: TrustLevelTrusted},
					{Kind: DiscoveryKindSkill, ID: "trusted-b", Name: "Trusted B", TrustLevel: TrustLevelTrusted},
				},
			},
			// Untrusted source
			&mockDiscoverySource{
				name: "remote-repo",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "untrusted-a", Name: "Untrusted A", TrustLevel: TrustLevelUntrusted},
				},
			},
			// MCP source
			&mockDiscoverySource{
				name: "mcp-api",
				kind: DiscoveryKindMCP,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindMCP, ID: "mcp-a", Name: "MCP A", URL: "https://example.com"},
				},
			},
		},
	}

	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	// Trusted skills in Skills
	if len(reg.Skills) != 2 {
		t.Fatalf("expected 2 trusted skills, got %d", len(reg.Skills))
	}

	// Untrusted skills in PendingSkills
	if len(reg.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill, got %d", len(reg.PendingSkills))
	}

	// MCPs in DynamicMCPs
	if len(reg.DynamicMCPs) != 1 {
		t.Fatalf("expected 1 DynamicMCP, got %d", len(reg.DynamicMCPs))
	}

	// Verify no cross-contamination
	if _, ok := reg.Skills["untrusted-a"]; ok {
		t.Fatal("untrusted skill should NOT be in Skills")
	}
	if _, ok := reg.PendingSkills["trusted-a"]; ok {
		t.Fatal("trusted skill should NOT be in PendingSkills")
	}
}

// ─── Integration: Repeated SyncAll updates existing entries ────────────────

// TestIntegration_RepeatedSyncAllUpdates verifies that running SyncAll
// multiple times (as the periodic ticker would) correctly updates existing
// entries rather than duplicating or losing them.
func TestIntegration_RepeatedSyncAllUpdates(t *testing.T) {
	reg := newDiscoveryTestRegistry()

	source := &mockDiscoverySource{
		name: "remote-skills",
		kind: DiscoveryKindSkill,
		items: []DiscoveredItem{
			{Kind: DiscoveryKindSkill, ID: "skill-1", Name: "Skill 1 v1", TrustLevel: TrustLevelUntrusted},
		},
	}
	d := &DiscoverySyncer{
		registry: reg,
		sources:  []DiscoverySource{source},
	}

	// First sync
	d.SyncAll()
	reg.Mu.RLock()
	if len(reg.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill after first sync, got %d", len(reg.PendingSkills))
	}
	if reg.PendingSkills["skill-1"].Name != "Skill 1 v1" {
		t.Fatalf("expected name 'Skill 1 v1', got %q", reg.PendingSkills["skill-1"].Name)
	}
	reg.Mu.RUnlock()

	// Update the source data and sync again
	source.items = []DiscoveredItem{
		{Kind: DiscoveryKindSkill, ID: "skill-1", Name: "Skill 1 v2", TrustLevel: TrustLevelUntrusted},
		{Kind: DiscoveryKindSkill, ID: "skill-2", Name: "Skill 2", TrustLevel: TrustLevelUntrusted},
	}
	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if len(reg.PendingSkills) != 2 {
		t.Fatalf("expected 2 pending skills after second sync, got %d", len(reg.PendingSkills))
	}
	if reg.PendingSkills["skill-1"].Name != "Skill 1 v2" {
		t.Fatalf("expected updated name 'Skill 1 v2', got %q", reg.PendingSkills["skill-1"].Name)
	}
	if reg.PendingSkills["skill-2"] == nil {
		t.Fatal("skill-2 not found in PendingSkills after second sync")
	}
}

// ─── Integration: DiscoverySyncer with nil context safety ──────────────────

// TestIntegration_DiscoverySyncerNilContextSafety verifies that SyncAll
// does not panic when the syncer's context is nil (defensive — the syncer
// should handle this gracefully).
func TestIntegration_DiscoverySyncerNilContextSafety(t *testing.T) {
	reg := newDiscoveryTestRegistry()
	d := &DiscoverySyncer{
		registry: reg,
		sources: []DiscoverySource{
			&mockDiscoverySource{
				name: "test",
				kind: DiscoveryKindSkill,
				items: []DiscoveredItem{
					{Kind: DiscoveryKindSkill, ID: "s1", Name: "S1", TrustLevel: TrustLevelUntrusted},
				},
			},
		},
		// ctx and cancel are nil — SyncAll should handle this
	}

	// Should not panic
	d.SyncAll()

	reg.Mu.RLock()
	defer reg.Mu.RUnlock()
	if len(reg.PendingSkills) != 1 {
		t.Fatalf("expected 1 pending skill, got %d", len(reg.PendingSkills))
	}
}
