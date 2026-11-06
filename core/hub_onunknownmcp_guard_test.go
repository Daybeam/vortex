package core

import (
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestOnUnknownMCP_NilInProduction is a guard test (audit M3, 2026-11-04).
//
// OnUnknownMCP is a designed seam in ContextHub that allows runtime dynamic
// MCP injection without pre-registration in config.json. It is intentionally
// nil in all production code paths — NewContextHub never assigns this field.
//
// Wiring this callback is a feature addition with security implications
// (auto-mounting unknown MCPs could be exploited). This test asserts the nil
// state to prevent accidental activation without a deliberate security review.
//
// If a future change wires OnUnknownMCP in NewContextHub or any production
// construction path, this test will fail — forcing the author to acknowledge
// the security decision and update this test.
func TestOnUnknownMCP_NilInProduction(t *testing.T) {
	reg := &config.Registry{
		Providers:   make(map[string]*config.ProviderConfig),
		MCPs:        make(map[string]*config.MCPDef),
		DynamicMCPs: make(map[string]*config.MCPDef),
	}
	graph := &schemas.TaskGraph{}

	hub := NewContextHub(reg, graph, nil)

	if hub.OnUnknownMCP != nil {
		t.Fatal(
			"OnUnknownMCP is non-nil in a hub constructed via NewContextHub.\n" +
				"This means a production code path now assigns the auto-discovery callback.\n" +
				"Auto-mounting unknown MCPs has security implications — an attacker could\n" +
				"reference a malicious MCP ID and have it auto-mounted with full tool access.\n" +
				"Before proceeding: (1) review the security implications, (2) ensure the\n" +
				"discovery source is trusted, (3) update this test to reflect the deliberate\n" +
				"design decision, and (4) update gap G7 in CALL_CHAINS.md §8.",
		)
	}
}
