package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestResolveToolMCP_RestrictedBindingDoesNotBypassAllowlist is the regression
// test for AUTH-2 / G4 (2026-10-23): a RESTRICTED binding (`AllowedTools`
// non-empty) is authorized ONLY for its explicit allow-list. Before the fix,
// ResolveToolMCP Tier 1 matched the MCP's exposure surface (`AvailableTools`)
// regardless of the binding, and Tier 3's unknown-name fallback routed to any
// healthy binding — so a call to an exposed-but-unauthorized tool resolved to
// the restricted binding. Authorization had collapsed into exposure.
func TestResolveToolMCP_RestrictedBindingDoesNotBypassAllowlist(t *testing.T) {
	hub := toggleHub() // active_mcp exposes a_tool + b_tool
	s := &Spawner{}

	restricted := []config.MCPBinding{{MCPID: "active_mcp", AllowedTools: []string{"a_tool"}}}

	// Authorized: in the allow-list -> resolves.
	if got := s.ResolveToolMCP("a_tool", restricted, hub); got != "active_mcp" {
		t.Fatalf("authorized tool must resolve to active_mcp, got %q", got)
	}
	// Exposed by the MCP but OUTSIDE the allow-list -> must NOT resolve.
	if got := s.ResolveToolMCP("b_tool", restricted, hub); got != "" {
		t.Fatalf("b_tool is exposed but unauthorized; must NOT route (got %q)", got)
	}
	// Unknown name with only a restricted binding -> fail closed (no Tier-3 route).
	if got := s.ResolveToolMCP("mystery_tool", restricted, hub); got != "" {
		t.Fatalf("unknown name with only a restricted binding must fail closed (got %q)", got)
	}
	// Unrestricted binding keeps the exposure behavior (no regression).
	unrestricted := []config.MCPBinding{{MCPID: "active_mcp"}}
	if got := s.ResolveToolMCP("b_tool", unrestricted, hub); got != "active_mcp" {
		t.Fatalf("unrestricted binding must still resolve an exposed tool, got %q", got)
	}
}