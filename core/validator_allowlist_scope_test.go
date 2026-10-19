package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestResolveFinalMCPs_BoundMCPIgnoresPerStepAllowlist documents a real
// limitation (2026-10-18): when a step passes additional_tool_allowlists for
// an MCP the role ALREADY binds, the per-step list is silently discarded.
//
// Root cause: ResolveToolBindings (config/registry_impl.go:132-136) only
// inserts an extra binding when `!exists`:
//
//	for _, b := range extra {
//	    if _, exists := effective[b.MCPID]; !exists {
//	        effective[b.MCPID] = b
//	        }
//	}
//
// effective is pre-seeded from role.BoundMCPBindings, so a bound MCP always
// wins. That makes per-step narrowing impossible without a code change — and
// per-step narrowing is exactly what a DAG decomposition needs (each sub-step
// should see only the tools for the app it touches).
//
// This test asserts the CURRENT (limiting) behavior so that if someone later
// "fixes" it, the test fails loudly and forces a conscious review of the
// privilege-escalation angle (a wider merge could widen a restricted binding).
func TestResolveFinalMCPs_BoundMCPIgnoresPerStepAllowlist(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"appworld": {ID: "appworld"},
		},
		Roles: map[string]*config.Role{
			"appworld_agent": {
				ID:               "appworld_agent",
				AllowDynamicMCPs: false,
				BoundMCPBindings: []config.MCPBinding{
					// Unrestricted binding, exactly like config_appworld*.json
					{MCPID: "appworld", AllowedTools: []string{}},
				},
			},
		},
	}
	hub := NewContextHub(reg, nil, nil)

	// A DAG step tries to narrow itself to venmo-only tools.
	stepAllowlist := map[string][]string{
		"appworld": {"venmo__login", "venmo__show_sent_payment_requests"},
	}

	bindings, err := ResolveFinalMCPs(hub, "appworld_agent", nil, stepAllowlist)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}

	got := bindings[0]
	if len(got.AllowedTools) != 0 {
		t.Fatalf("per-step allowlist unexpectedly applied: %v — "+
			"if you changed ResolveToolBindings, re-read the privilege "+
			"escalation note above", got.AllowedTools)
	}
	t.Logf("confirmed: bound MCP %q keeps AllowedTools=%v (per-step narrowing silently dropped)",
		got.MCPID, got.AllowedTools)
}
