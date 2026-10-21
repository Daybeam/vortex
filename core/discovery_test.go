package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestParseScope(t *testing.T) {
	tests := []struct {
		name      string
		scopeStr  string
		roleID    string
		allowPool bool
		wantMode  string
		wantRole  string
		wantErr   bool
	}{
		{"self", "self", "my_role", false, "self", "my_role", false},
		{"empty defaults to self", "", "my_role", false, "self", "my_role", false},
		// SECURITY FIX (2026-10-21): role:<id> requires admin or self-inspection
		{"role denied for non-admin non-self", "role:target", "caller", false, "", "", true},
		{"role self-inspection allowed", "role:my_role", "my_role", false, "role", "my_role", false},
		{"role allowed for admin", "role:target", "caller", true, "role", "target", false},
		{"role empty ID", "role:", "caller", false, "", "", true},
		{"pool allowed", "pool", "", true, "pool", "", false},
		{"pool denied at parse", "pool", "", false, "pool", "", false}, // ParseScope doesn't enforce; enforceScope does
		{"invalid", "invalid", "", false, "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope, err := ParseScope(tt.scopeStr, tt.roleID, tt.allowPool)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if scope.Mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", scope.Mode, tt.wantMode)
			}
			if scope.RoleID != tt.wantRole {
				t.Errorf("roleID = %q, want %q", scope.RoleID, tt.wantRole)
			}
		})
	}
}

func TestEnforceScope(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"role_a": {
				ID: "role_a",
				BoundMCPBindings: []config.MCPBinding{
					{MCPID: "mcp1"},
					{MCPID: "mcp2"},
				},
			},
			"role_b": {
				ID: "role_b",
				BoundMCPBindings: []config.MCPBinding{
					{MCPID: "mcp3"},
				},
			},
		},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {ID: "mcp1"},
			"mcp2": {ID: "mcp2"},
			"mcp3": {ID: "mcp3"},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}

	t.Run("self scope returns role's MCPs", func(t *testing.T) {
		ids, err := enforceScope(reg, DiscoveryScope{Mode: "self", RoleID: "role_a"})
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 2 {
			t.Fatalf("expected 2 MCP IDs, got %d: %v", len(ids), ids)
		}
	})

	t.Run("role scope returns target role's MCPs", func(t *testing.T) {
		ids, err := enforceScope(reg, DiscoveryScope{Mode: "role", RoleID: "role_b"})
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0] != "mcp3" {
			t.Fatalf("expected [mcp3], got %v", ids)
		}
	})

	t.Run("pool scope denied without permission", func(t *testing.T) {
		_, err := enforceScope(reg, DiscoveryScope{Mode: "pool", AllowPool: false})
		if err == nil {
			t.Fatal("expected error for pool without permission")
		}
	})

	t.Run("pool scope allowed with permission", func(t *testing.T) {
		ids, err := enforceScope(reg, DiscoveryScope{Mode: "pool", AllowPool: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 3 {
			t.Fatalf("expected 3 MCP IDs, got %d", len(ids))
		}
	})

	t.Run("self scope with unknown role errors", func(t *testing.T) {
		_, err := enforceScope(reg, DiscoveryScope{Mode: "self", RoleID: "nonexistent"})
		if err == nil {
			t.Fatal("expected error for unknown role")
		}
	})
}

func TestDiscoverImpl_Tools(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"role_a": {
				ID: "role_a",
				BoundMCPBindings: []config.MCPBinding{
					{MCPID: "mcp1"},
				},
			},
		},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "search", Description: "Search the web"},
					{Name: "fetch", Description: "Fetch a URL"},
				},
			},
			"mcp2": {
				ID: "mcp2",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "execute", Description: "Execute code"},
				},
			},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}

	t.Run("self scope returns only role's tools", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "self", RoleID: "role_a"}, "tools", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 2 {
			t.Fatalf("expected 2 tools, got %d", len(result.Tools))
		}
		if result.Total != 2 {
			t.Errorf("total = %d, want 2", result.Total)
		}
	})

	t.Run("query filters tools", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "self", RoleID: "role_a"}, "tools", "search", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 1 || result.Tools[0].Name != "search" {
			t.Fatalf("expected [search], got %v", result.Tools)
		}
	})

	t.Run("pool scope returns all tools", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "pool", AllowPool: true}, "tools", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 3 {
			t.Fatalf("expected 3 tools, got %d", len(result.Tools))
		}
	})
}

func TestDiscoverImpl_Roles(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"role_a": {ID: "role_a", Name: "Role A", BaseCapability: "code"},
			"role_b": {ID: "role_b", Name: "Role B", BaseCapability: "plan"},
		},
		MCPs:        map[string]*config.MCPDef{},
		DynamicMCPs: map[string]*config.MCPDef{},
	}

	t.Run("self returns one role", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "self", RoleID: "role_a"}, "roles", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Roles) != 1 || result.Roles[0].ID != "role_a" {
			t.Fatalf("expected [role_a], got %v", result.Roles)
		}
	})

	t.Run("pool returns all roles", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "pool", AllowPool: true}, "roles", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Roles) != 2 {
			t.Fatalf("expected 2 roles, got %d", len(result.Roles))
		}
	})
}

func TestDiscoverImpl_MCPs(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"role_a": {
				ID: "role_a",
				BoundMCPBindings: []config.MCPBinding{{MCPID: "mcp1"}},
			},
		},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {ID: "mcp1", Trusted: true},
			"mcp2": {ID: "mcp2", Trusted: false},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}

	t.Run("self returns only role's MCPs", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "self", RoleID: "role_a"}, "mcps", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.MCPs) != 1 || result.MCPs[0].ID != "mcp1" {
			t.Fatalf("expected [mcp1], got %v", result.MCPs)
		}
	})

	t.Run("pool returns all MCPs", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "pool", AllowPool: true}, "mcps", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.MCPs) != 2 {
			t.Fatalf("expected 2 MCPs, got %d", len(result.MCPs))
		}
	})
}

// TestDiscoverMCPs_RedactsConnectionInfoForNonAdmin is the regression for
// security-audit issue 8 (2026-10-21): MCPSummary used to always return the
// MCP's URL/Command, leaking connection internals (endpoints, launch commands)
// to any caller, including public tier. Non-admin (self/role scope) must get
// redacted summaries; only admin (pool scope) sees URL/Command.
func TestDiscoverMCPs_RedactsConnectionInfoForNonAdmin(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"role_a": {
				ID:               "role_a",
				BoundMCPBindings: []config.MCPBinding{{MCPID: "mcp1"}},
			},
		},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:      "mcp1",
				URL:     "https://secret.internal/mcp",
				Command: "node /opt/secret/server.js",
				Trusted: true,
			},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}

	t.Run("non-admin self scope is redacted", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "self", RoleID: "role_a"}, "mcps", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.MCPs) != 1 {
			t.Fatalf("expected 1 MCP, got %d", len(result.MCPs))
		}
		if result.MCPs[0].URL != "" || result.MCPs[0].Command != "" {
			t.Fatalf("non-admin leak: url=%q command=%q", result.MCPs[0].URL, result.MCPs[0].Command)
		}
	})

	t.Run("admin pool scope keeps connection info", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "pool", AllowPool: true}, "mcps", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.MCPs) != 1 {
			t.Fatalf("expected 1 MCP, got %d", len(result.MCPs))
		}
		if result.MCPs[0].URL == "" || result.MCPs[0].Command == "" {
			t.Fatalf("admin should see connection info, got url=%q command=%q", result.MCPs[0].URL, result.MCPs[0].Command)
		}
	})
}

func TestDiscoverImpl_Pagination(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"role_a": {
				ID: "role_a",
				BoundMCPBindings: []config.MCPBinding{{MCPID: "mcp1"}},
			},
		},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "tool_a"},
					{Name: "tool_b"},
					{Name: "tool_c"},
					{Name: "tool_d"},
					{Name: "tool_e"},
				},
			},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}

	t.Run("page 1 size 2", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "self", RoleID: "role_a"}, "tools", "", 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 2 {
			t.Fatalf("expected 2 tools, got %d", len(result.Tools))
		}
		if result.Total != 5 {
			t.Errorf("total = %d, want 5", result.Total)
		}
	})

	t.Run("page 3 size 2 returns 1", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "self", RoleID: "role_a"}, "tools", "", 3, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(result.Tools))
		}
	})

	t.Run("page beyond range returns empty", func(t *testing.T) {
		result, err := DiscoverImpl(reg, DiscoveryScope{Mode: "self", RoleID: "role_a"}, "tools", "", 10, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Tools) != 0 {
			t.Fatalf("expected 0 tools, got %d", len(result.Tools))
		}
	})
}

func TestFindSkillsImpl(t *testing.T) {
	reg := &config.Registry{
		Skills: map[string]*config.Skill{
			"code_search": {
				ID:          "code_search",
				Name:        "Code Search",
				Capability:  "code",
				Description: "Search codebase for patterns",
				Provides:    []string{"search", "grep"},
			},
			"code_review": {
				ID:          "code_review",
				Name:        "Code Review",
				Capability:  "code",
				Description: "Review code for issues",
				Provides:    []string{"review", "lint"},
			},
			"plan_decompose": {
				ID:          "plan_decompose",
				Name:        "Task Decomposition",
				Capability:  "plan",
				Description: "Decompose tasks into steps",
				Provides:    []string{"decompose", "plan"},
			},
		},
	}

	t.Run("capability match", func(t *testing.T) {
		result, err := FindSkillsImpl(reg, "", "code", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Skills) != 2 {
			t.Fatalf("expected 2 skills, got %d", len(result.Skills))
		}
		// Both should match by capability
		for _, s := range result.Skills {
			if s.MatchReason != "capability" {
				t.Errorf("expected match_reason 'capability', got %q", s.MatchReason)
			}
		}
	})

	t.Run("provides match", func(t *testing.T) {
		result, err := FindSkillsImpl(reg, "", "", "search", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Skills) != 1 || result.Skills[0].ID != "code_search" {
			t.Fatalf("expected [code_search], got %v", result.Skills)
		}
		if result.Skills[0].MatchReason != "provides" {
			t.Errorf("expected match_reason 'provides', got %q", result.Skills[0].MatchReason)
		}
	})

	t.Run("query name match", func(t *testing.T) {
		result, err := FindSkillsImpl(reg, "decomposition", "", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Skills) != 1 || result.Skills[0].ID != "plan_decompose" {
			t.Fatalf("expected [plan_decompose], got %v", result.Skills)
		}
		if result.Skills[0].MatchReason != "name" {
			t.Errorf("expected match_reason 'name', got %q", result.Skills[0].MatchReason)
		}
	})

	t.Run("query description match", func(t *testing.T) {
		result, err := FindSkillsImpl(reg, "patterns", "", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Skills) != 1 || result.Skills[0].ID != "code_search" {
			t.Fatalf("expected [code_search], got %v", result.Skills)
		}
		if result.Skills[0].MatchReason != "description" {
			t.Errorf("expected match_reason 'description', got %q", result.Skills[0].MatchReason)
		}
	})

	t.Run("priority sorting: capability before name", func(t *testing.T) {
		// "code" matches code_search by capability AND code_review by capability
		// "review" matches code_review by provides
		// Combined: both match, but capability has higher priority
		result, err := FindSkillsImpl(reg, "review", "code", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Skills) != 2 {
			t.Fatalf("expected 2 skills, got %d", len(result.Skills))
		}
		// Both match by capability (higher priority than name/description)
		for _, s := range result.Skills {
			if s.MatchReason != "capability" {
				t.Errorf("expected match_reason 'capability', got %q", s.MatchReason)
			}
		}
	})

	t.Run("empty filters return empty", func(t *testing.T) {
		result, err := FindSkillsImpl(reg, "", "", "", 1, 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Skills) != 0 {
			t.Fatalf("expected 0 skills, got %d", len(result.Skills))
		}
	})
}

// TestDiscoverTools_AllowedToolsNarrowing is the regression for issue 6
// (2026-10-21): discoverTools previously returned ALL FullToolDefinitions
// for each MCP, ignoring AllowedTools narrowing. Now it respects the role's
// AllowedTools filter.
func TestDiscoverTools_AllowedToolsNarrowing(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"narrowed_role": {
				ID: "narrowed_role",
				BoundMCPBindings: []config.MCPBinding{
					{
						MCPID:        "mcp_tools",
						AllowedTools: []string{"tool_a", "tool_b"}, // narrowed — tool_c excluded
					},
				},
			},
		},
		MCPs: map[string]*config.MCPDef{
			"mcp_tools": {
				ID: "mcp_tools",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "tool_a", Description: "Tool A"},
					{Name: "tool_b", Description: "Tool B"},
					{Name: "tool_c", Description: "Tool C (should be filtered out)"},
				},
			},
		},
		DynamicMCPs: map[string]*config.MCPDef{},
	}

	scope := DiscoveryScope{Mode: "self", RoleID: "narrowed_role"}
	result, err := DiscoverImpl(reg, scope, "tools", "", 1, 20)
	if err != nil {
		t.Fatalf("DiscoverImpl failed: %v", err)
	}

	if result.Total != 2 {
		t.Fatalf("SECURITY: expected 2 tools (tool_a + tool_b), got %d — AllowedTools narrowing not applied", result.Total)
	}

	// Verify tool_c is NOT in the results
	for _, tool := range result.Tools {
		if tool.Name == "tool_c" {
			t.Error("SECURITY: tool_c should be filtered out by AllowedTools narrowing")
		}
	}
}
