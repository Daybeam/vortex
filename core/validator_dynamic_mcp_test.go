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

	got, err := ResolveFinalMCPs(hub, "appworld_agent", []string{"appworld"}, nil, true)
	if err != nil {
		t.Fatalf("naming an already-bound MCP must not count as a dynamic mount, got error: %v", err)
	}
	if len(got) != 1 || got[0].MCPID != "appworld" {
		t.Fatalf("expected the single bound MCP, got %+v", got)
	}
}

// TestResolveFinalMCPs_GenuinelyNewMCPStillNeedsPermission keeps the guard
// meaningful: non-admin callers must not mount dynamic MCPs.
// Admin callers CAN mount on any role (per security audit: tier check, not role flag).
func TestResolveFinalMCPs_GenuinelyNewMCPStillNeedsPermission(t *testing.T) {
	hub := newResolveHub()

	// Non-admin: must be rejected
	if _, err := ResolveFinalMCPs(hub, "appworld_agent", []string{"other"}, nil, false); err == nil {
		t.Fatal("non-admin caller must not mount dynamic MCPs")
	}

	// Admin: must be allowed even on roles with AllowDynamicMCPs=false
	got, err := ResolveFinalMCPs(hub, "appworld_agent", []string{"other"}, nil, true)
	if err != nil {
		t.Fatalf("admin caller must be able to mount dynamic MCPs on any role, got: %v", err)
	}
	// appworld_agent binds "appworld" + additional "other" = 2 bindings
	if len(got) != 2 {
		t.Fatalf("expected 2 bindings (appworld + other), got %d: %+v", len(got), got)
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
		map[string][]string{"appworld": {"venmo__login", "venmo__remind_payment_request"}}, true)
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

// TestResolveFinalMCPs_NonAdminRejectsDynamicMCP is the security regression for
// issue 3 (2026-10-21): even with AllowDynamicMCPs=true (like sop_generator),
// a non-admin submitter must NOT be able to mount dynamic MCPs.
// Attack path: public caller → orchestrator_submit_task with role_id="sop_generator"
// + additional_mcps=["high_risk_mcp"] → ResolveFinalMCPs must reject.
func TestResolveFinalMCPs_NonAdminRejectsDynamicMCP(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"sop_generator": {
				ID:               "sop_generator",
				AllowDynamicMCPs: true, // the vulnerable flag
			},
		},
		MCPs: map[string]*config.MCPDef{
			"high_risk_mcp": {ID: "high_risk_mcp"},
		},
	}
	hub := NewContextHub(reg, nil, nil)

	_, err := ResolveFinalMCPs(hub, "sop_generator", []string{"high_risk_mcp"}, nil, false)
	if err == nil {
		t.Fatal("SECURITY: non-admin submitter must NOT mount dynamic MCPs even with AllowDynamicMCPs=true")
	}
}

// TestResolveFinalMCPs_AdminCanMountDynamicMCP verifies the fix doesn't
// break the legitimate admin path: admin + AllowDynamicMCPs=true → allowed.
func TestResolveFinalMCPs_AdminCanMountDynamicMCP(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"sop_generator": {
				ID:               "sop_generator",
				AllowDynamicMCPs: true,
			},
		},
		MCPs: map[string]*config.MCPDef{
			"needed_mcp": {ID: "needed_mcp"},
		},
	}
	hub := NewContextHub(reg, nil, nil)

	got, err := ResolveFinalMCPs(hub, "sop_generator", []string{"needed_mcp"}, nil, true)
	if err != nil {
		t.Fatalf("admin + AllowDynamicMCPs=true must allow dynamic mount, got: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(got))
	}
}
