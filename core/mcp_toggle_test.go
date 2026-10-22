package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// Tests for the operator MCP / tool toggle. See docs/completed/2026-10-22/MCP_TOOL_TOGGLE_DESIGN.md.

// toggleRegistry builds a registry covering the toggle cases:
//   - active_mcp      : fully enabled (a_tool, b_tool)
//   - disabled_mcp    : MCP-level off (c_tool)
//   - denied_tool_mcp : MCP on, but "drop" is in the tool deny-list (keep, drop)
func toggleRegistry() *config.Registry {
	return &config.Registry{
		Roles: map[string]*config.Role{},
		MCPs: map[string]*config.MCPDef{
			"active_mcp": {
				ID:             "active_mcp",
				AvailableTools: []string{"a_tool", "b_tool"},
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "a_tool"}, {Name: "b_tool"},
				},
			},
			"disabled_mcp": {
				ID:             "disabled_mcp",
				Disabled:       true,
				AvailableTools: []string{"c_tool"},
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "c_tool"},
				},
			},
			"denied_tool_mcp": {
				ID:             "denied_tool_mcp",
				AvailableTools: []string{"keep", "drop"},
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "keep"}, {Name: "drop"},
				},
				DisabledTools: []string{"drop"},
			},
		},
	}
}

func toggleHub() *ContextHub { return NewContextHub(toggleRegistry(), nil, nil) }

func toolNames(defs []schemas.ToolDefinition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func TestMCPToggle_Predicates(t *testing.T) {
	if mcpActive(nil) {
		t.Fatal("nil MCP must be inactive")
	}
	if !mcpActive(&config.MCPDef{}) {
		t.Fatal("an MCP with no flag must be active (default)")
	}
	if mcpActive(&config.MCPDef{Disabled: true}) {
		t.Fatal("a Disabled MCP must be inactive")
	}

	m := &config.MCPDef{DisabledTools: []string{"x"}}
	if toolEnabled(m, "x") {
		t.Fatal("a denied tool must be disabled")
	}
	if !toolEnabled(m, "y") {
		t.Fatal("a non-denied tool must stay enabled")
	}
	if toolEnabled(&config.MCPDef{Disabled: true}, "y") {
		t.Fatal("every tool of a disabled MCP must be disabled")
	}
}

func TestMCPToggle_VisibleTools(t *testing.T) {
	hub := toggleHub()

	// A denied tool is dropped; its sibling survives.
	got := toolNames(visibleTools(hub.GetMCP("denied_tool_mcp"), config.MCPBinding{MCPID: "denied_tool_mcp"}))
	if containsStr(got, "drop") {
		t.Fatalf("denied tool must not be visible, got %v", got)
	}
	if !containsStr(got, "keep") {
		t.Fatalf("non-denied tool must remain visible, got %v", got)
	}

	// A disabled MCP exposes nothing at all.
	if got := visibleTools(hub.GetMCP("disabled_mcp"), config.MCPBinding{MCPID: "disabled_mcp"}); len(got) != 0 {
		t.Fatalf("a disabled MCP must expose nothing, got %v", toolNames(got))
	}

	// The binding allow-list still narrows (toggle does not widen).
	restricted := visibleTools(hub.GetMCP("active_mcp"), config.MCPBinding{MCPID: "active_mcp", AllowedTools: []string{"a_tool"}})
	if len(restricted) != 1 || restricted[0].Name != "a_tool" {
		t.Fatalf("binding allow-list must narrow, got %v", toolNames(restricted))
	}
}

func TestMCPToggle_RouteDropsDisabledMCP(t *testing.T) {
	router := NewToolRouter(toggleRegistry(), nil)
	routed := router.Route(context.Background(), RouteRequest{
		Task: "do something",
		Bindings: []config.MCPBinding{
			{MCPID: "active_mcp"},
			{MCPID: "disabled_mcp"},
		},
	})
	for _, b := range routed {
		if b.MCPID == "disabled_mcp" {
			t.Fatalf("Route must drop a disabled MCP, got bindings %v", routed)
		}
	}
}

func TestMCPToggle_ResolveToolMCPRefusesDisabled(t *testing.T) {
	hub := toggleHub()
	s := &Spawner{}

	if got := s.ResolveToolMCP("a_tool", []config.MCPBinding{{MCPID: "active_mcp"}}, hub); got != "active_mcp" {
		t.Fatalf("an active declared tool must resolve, got %q", got)
	}
	if got := s.ResolveToolMCP("c_tool", []config.MCPBinding{{MCPID: "disabled_mcp"}}, hub); got != "" {
		t.Fatalf("a disabled MCP must not resolve any tool, got %q", got)
	}
	if got := s.ResolveToolMCP("drop", []config.MCPBinding{{MCPID: "denied_tool_mcp"}}, hub); got != "" {
		t.Fatalf("a denied tool must not resolve (incl. the Tier-3 fallback), got %q", got)
	}
	if got := s.ResolveToolMCP("keep", []config.MCPBinding{{MCPID: "denied_tool_mcp"}}, hub); got != "denied_tool_mcp" {
		t.Fatalf("a non-denied tool must resolve, got %q", got)
	}
}

// The toggle must survive config serialization; absent fields keep the safe
// defaults (active, empty deny-list) so existing configs keep working.
func TestMCPToggle_ConfigRoundTrip(t *testing.T) {
	data, err := json.Marshal(config.MCPDef{ID: "m1", Disabled: true, DisabledTools: []string{"x", "y"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out config.MCPDef
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.Disabled {
		t.Fatal("Disabled lost across JSON round-trip")
	}
	if len(out.DisabledTools) != 2 || out.DisabledTools[0] != "x" {
		t.Fatalf("DisabledTools lost across JSON round-trip: %v", out.DisabledTools)
	}

	var def config.MCPDef
	if err := json.Unmarshal([]byte(`{"id":"m2"}`), &def); err != nil {
		t.Fatalf("unmarshal default: %v", err)
	}
	if def.Disabled || len(def.DisabledTools) != 0 {
		t.Fatalf("absent fields must default to active + empty deny-list, got %+v", def)
	}
}
