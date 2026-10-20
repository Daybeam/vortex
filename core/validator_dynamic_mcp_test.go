package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// plannerRole returns a role shaped like AppWorld's executor: it binds one MCP
// and forbids dynamic mounts.
func plannerRole() *config.Role {
	return &config.Role{
		ID:               "appworld_agent",
		AllowDynamicMCPs: false,
		BoundMCPBindings: []config.MCPBinding{
			{MCPID: "appworld", AllowedTools: []string{}},
		},
	}
}

func newResolveHub() *ContextHub {
	reg := &config.Registry{
		Roles: map[string]*config.Role{"appworld_agent": plannerRole()},
		MCPs: map[string]*config.MCPDef{
			"appworld": {ID: "appworld"},
			"other":    {ID: "other"},
		},
	}
	return NewContextHub(reg, nil, nil)
}

// TestResolveFinalMCPs_BoundMCPIsNotDynamic is the regression for the failure
// that killed every planner-produced step on 2026-10-19: the planner writes
// mcp_id on each step to name the MCP it chose, ParsePlanDAG turns that into
// AdditionalMCPs, and the old guard rejected it because the executor role sets
// allow_dynamic_mcps=false — even though the role already binds that exact MCP.
func TestResolveFinalMCPs_BoundMCPIsNotDynamic(t *testing.T) {
	hub := newResolveHub()

	got, err := ResolveFinalMCPs(hub, "appworld_agent", []string{"appworld"}, nil)
	if err != nil {
		t.Fatalf("naming an already-bound MCP must not count as a dynamic mount, got error: %v", err)
	}
	if len(got) != 1 || got[0].MCPID != "appworld" {
		t.Fatalf("expected the single bound MCP, got %+v", got)
	}
}

// TestResolveFinalMCPs_GenuinelyNewMCPStillNeedsPermission keeps the guard
// meaningful: the fix must not become "any MCP may be mounted".
func TestResolveFinalMCPs_GenuinelyNewMCPStillNeedsPermission(t *testing.T) {
	hub := newResolveHub()

	if _, err := ResolveFinalMCPs(hub, "appworld_agent", []string{"other"}, nil); err == nil {
		t.Fatal("mounting an MCP the role does not bind must still require allow_dynamic_mcps")
	}
}

// TestResolveFinalMCPs_PlannerAllowlistNarrowsBoundMCP checks that the
// planner's tool selection survives this path. ResolveToolBindings already
// knows how to narrow (2026-10-18), but it was unreachable while the guard
// above returned first — so the planner's careful tool lists had never taken
// effect on a real run.
func TestResolveFinalMCPs_PlannerAllowlistNarrowsBoundMCP(t *testing.T) {
	hub := newResolveHub()

	got, err := ResolveFinalMCPs(hub, "appworld_agent",
		[]string{"appworld"},
		map[string][]string{"appworld": {"venmo__login", "venmo__remind_payment_request"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(got))
	}
	if len(got[0].AllowedTools) != 2 {
		t.Fatalf("planner allowlist was dropped; want 2 tools, got %+v", got[0].AllowedTools)
	}
}
