package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestOnUnknownMCP_WiredInProduction is a guard test (updated 2026-11-09).
//
// OnUnknownMCP is a designed seam in ContextHub that allows runtime dynamic
// MCP injection without pre-registration in config.json. As of 2026-11-09,
// NewContextHub now wires this callback by default to look up DynamicMCPs
// in the registry — enabling the DiscoverySyncer to populate DynamicMCPs
// from external discovery sources (HTTP APIs, GitHub repos, local dirs).
//
// Security: discovered MCPs always carry MountPolicy=admin_only and
// Source=external_dynamic. The mount-policy gate in validator.go and
// ServerRiskyMCP checks remain in effect. OnUnknownMCP only reads from
// the in-memory DynamicMCPs cache — no network calls in the request path.
//
// See docs/plan/DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md §4.6 and §5.2.
func TestOnUnknownMCP_WiredInProduction(t *testing.T) {
	reg := &config.Registry{
		Providers:   make(map[string]*config.ProviderConfig),
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	graph := &schemas.TaskGraph{}

	hub := NewContextHub(reg, graph, nil)

	if hub.OnUnknownMCP == nil {
		t.Fatal(
			"OnUnknownMCP is nil in a hub constructed via NewContextHub.\n" +
				"As of 2026-11-09, NewContextHub wires this callback to look up\n" +
				"DynamicMCPs from the registry. A nil callback means the wiring\n" +
				"was removed — verify that DiscoverySyncer still functions and\n" +
				"that the security constraints (admin_only, external_dynamic)\n" +
				"are still enforced. See DISCOVERY_SOURCE_ARCHITECTURE_DESIGN.md §4.6.",
		)
	}

	// Verify the callback reads from DynamicMCPs.
	testMCP := &config.MCPDef{
		ID:          "test-dynamic-mcp",
		URL:         "https://example.com/mcp",
		MountPolicy: config.MountPolicyAdminOnly,
		Source:      config.SourceExternalDynamic,
	}
	reg.Mu.Lock()
	reg.DynamicMCPs["test-dynamic-mcp"] = testMCP
	reg.Mu.Unlock()

	result := hub.OnUnknownMCP("test-dynamic-mcp")
	if result == nil {
		t.Fatal("OnUnknownMCP returned nil for a DynamicMCPs entry that exists")
	}
	if result.ID != "test-dynamic-mcp" {
		t.Fatalf("OnUnknownMCP returned wrong MCP: got %q, want %q", result.ID, "test-dynamic-mcp")
	}

	// Verify the callback returns nil for unknown MCP IDs.
	if result := hub.OnUnknownMCP("nonexistent-mcp"); result != nil {
		t.Fatalf("OnUnknownMCP returned non-nil for unknown MCP ID: %v", result)
	}
}

// TestOnUnknownMCP_NilRegistry ensures NewContextHub does not panic when
// the registry is nil (defensive — some test paths construct hubs with nil).
func TestOnUnknownMCP_NilRegistry(t *testing.T) {
	graph := &schemas.TaskGraph{}

	// Should not panic even with nil registry.
	hub := NewContextHub(nil, graph, nil)

	// OnUnknownMCP may be nil when registry is nil — that's acceptable.
	// The nil-checks at validator.go:290 and validator.go:481 guard the call.
	_ = hub.OnUnknownMCP
}
