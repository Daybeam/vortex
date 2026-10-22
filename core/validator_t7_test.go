package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// T7 regression: execution-time tool allowlist intersection.
//
// After ParsePlanDAG accepts tools as "unverified" on a cold engine, the
// execution-time ResolveFinalMCPs must intersect the planner-specified
// allowlist with the MCP's discovered AvailableTools. This filters out
// invented tool names. If the intersection is empty, it falls back to full
// access (unrestricted) rather than blocking the step.

func TestResolveFinalMCPs_T7Intersection_FiltersInventedTools(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"appworld": {
				ID: "appworld",
				// Discovered tools (after MCP handshake)
				AvailableTools: []string{"venmo__login", "venmo__list_payments", "search__query"},
			},
		},
		Roles: map[string]*config.Role{
			"agent": {
				ID:               "agent",
				AllowDynamicMCPs: true,
			},
		},
	}
	hub := NewContextHub(reg, nil, nil)

	// Planner specified tools, some of which are invented (don't exist).
	// This simulates a cold-engine plan that was accepted as "unverified".
	allowlists := map[string][]string{
		"appworld": {"venmo__login", "venmo__list_payments", "venmo__FAKE_TOOL", "invented__nope"},
	}

	bindings, err := ResolveFinalMCPs(hub, "agent", []string{"appworld"}, allowlists, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, b := range bindings {
		if b.MCPID != "appworld" {
			continue
		}
		// Invented tools should be filtered out
		for _, tool := range b.AllowedTools {
			if tool == "venmo__FAKE_TOOL" || tool == "invented__nope" {
				t.Errorf("invented tool %q should be filtered by intersection", tool)
			}
		}
		// Real tools should be preserved
		hasLogin := false
		hasList := false
		for _, tool := range b.AllowedTools {
			if tool == "venmo__login" {
				hasLogin = true
			}
			if tool == "venmo__list_payments" {
				hasList = true
			}
		}
		if !hasLogin {
			t.Errorf("real tool 'venmo__login' should be preserved after intersection")
		}
		if !hasList {
			t.Errorf("real tool 'venmo__list_payments' should be preserved after intersection")
		}
	}
}

func TestResolveFinalMCPs_T7Intersection_EmptyIntersectionFallsBackToFull(t *testing.T) {
	// If NONE of the planned tools exist in the discovered set, fall back
	// to full access (unrestricted) rather than blocking the step.
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"appworld": {
				ID:             "appworld",
				AvailableTools: []string{"real_tool_1", "real_tool_2"},
			},
		},
		Roles: map[string]*config.Role{
			"agent": {
				ID:               "agent",
				AllowDynamicMCPs: true,
			},
		},
	}
	hub := NewContextHub(reg, nil, nil)

	// All planned tools are invented — none exist in AvailableTools.
	allowlists := map[string][]string{
		"appworld": {"fake_tool_1", "fake_tool_2"},
	}

	bindings, err := ResolveFinalMCPs(hub, "agent", []string{"appworld"}, allowlists, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, b := range bindings {
		if b.MCPID != "appworld" {
			continue
		}
		// Empty intersection → fall back to full access (empty = unrestricted)
		if len(b.AllowedTools) != 0 {
			t.Errorf("empty intersection should fall back to full access (empty allowlist), got %v", b.AllowedTools)
		}
	}
}

func TestResolveFinalMCPs_T7Intersection_NoAvailableToolsSkipsIntersection(t *testing.T) {
	// If AvailableTools is empty (MCP not yet discovered at execution time
	// either), skip the intersection — keep the planner's allowlist as-is.
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"appworld": {
				ID:             "appworld",
				AvailableTools: nil, // still cold
			},
		},
		Roles: map[string]*config.Role{
			"agent": {
				ID:               "agent",
				AllowDynamicMCPs: true,
			},
		},
	}
	hub := NewContextHub(reg, nil, nil)

	allowlists := map[string][]string{
		"appworld": {"venmo__login", "venmo__list_payments"},
	}

	bindings, err := ResolveFinalMCPs(hub, "agent", []string{"appworld"}, allowlists, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, b := range bindings {
		if b.MCPID != "appworld" {
			continue
		}
		// No AvailableTools → no intersection → keep planner's allowlist
		if len(b.AllowedTools) != 2 {
			t.Errorf("with no AvailableTools, planner allowlist should be kept as-is, got %v", b.AllowedTools)
		}
	}
}

func TestResolveFinalMCPs_T7Intersection_AllToolsMatchIsNoOp(t *testing.T) {
	// If all planned tools exist in the discovered set, the intersection
	// is a no-op (same tools come out).
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"appworld": {
				ID:             "appworld",
				AvailableTools: []string{"tool_a", "tool_b", "tool_c"},
			},
		},
		Roles: map[string]*config.Role{
			"agent": {
				ID:               "agent",
				AllowDynamicMCPs: true,
			},
		},
	}
	hub := NewContextHub(reg, nil, nil)

	allowlists := map[string][]string{
		"appworld": {"tool_a", "tool_b"},
	}

	bindings, err := ResolveFinalMCPs(hub, "agent", []string{"appworld"}, allowlists, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, b := range bindings {
		if b.MCPID != "appworld" {
			continue
		}
		if len(b.AllowedTools) != 2 {
			t.Errorf("all-matching intersection should be no-op, got %d tools: %v", len(b.AllowedTools), b.AllowedTools)
		}
	}
}
