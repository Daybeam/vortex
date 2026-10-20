package core

import (
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestStickyUnionBindings_AddsCalledToolsBack verifies that tools called in
// prior turns are added back to the bindings after re-routing.
func TestStickyUnionBindings_AddsCalledToolsBack(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				AvailableTools: []string{
					"venmo__show_sent_payment_requests",
					"venmo__show_received_payment_requests",
					"venmo__send_payment",
					"venmo__get_balance",
				},
			},
		},
	}

	spawner := &Spawner{registry: registry}

	bindings := []config.MCPBinding{
		{
			MCPID:        "mcp1",
			AllowedTools: []string{"venmo__show_received_payment_requests", "venmo__send_payment"},
		},
	}
	initialBindings := []config.MCPBinding{
		{
			MCPID:        "mcp1",
			AllowedTools: []string{"venmo__show_sent_payment_requests", "venmo__show_received_payment_requests", "venmo__send_payment", "venmo__get_balance"},
		},
	}
	calledTools := map[string]bool{
		"venmo__show_sent_payment_requests": true,
	}

	result := spawner.stickyUnionBindings(bindings, initialBindings, calledTools)

	allowed := make(map[string]bool)
	for _, tool := range result[0].AllowedTools {
		allowed[tool] = true
	}
	if !allowed["venmo__show_sent_payment_requests"] {
		t.Error("previously-called tool venmo__show_sent_payment_requests was not restored by sticky union")
	}
	if !allowed["venmo__show_received_payment_requests"] {
		t.Error("existing tool venmo__show_received_payment_requests was lost")
	}
	if !allowed["venmo__send_payment"] {
		t.Error("existing tool venmo__send_payment was lost")
	}
}

// TestStickyUnionBindings_DoesNotWidenBeyondAllowlist verifies that a tool
// in the MCP's AvailableTools but NOT in the original binding's allowlist
// is NOT added back, even if it was called. This is the core security
// guarantee: the sticky union boundary is the original allowlist, not
// AvailableTools.
func TestStickyUnionBindings_DoesNotWidenBeyondAllowlist(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "8",
				AvailableTools: []string{
					"tool_a",
					"tool_b",
					"tool_c", // in AvailableTools but NOT in original allowlist
				},
			},
		},
	}

	spawner := &Spawner{registry: registry}

	bindings := []config.MCPBinding{
		{MCPID: "8", AllowedTools: []string{"tool_a"}},
	}
	// Original binding was restricted to tool_a and tool_b only.
	initialBindings := []config.MCPBinding{
		{MCPID: "8", AllowedTools: []string{"tool_a", "tool_b"}},
	}
	// tool_c was called but was never in the original restricted allowlist.
	calledTools := map[string]bool{"tool_c": true}

	result := spawner.stickyUnionBindings(bindings, initialBindings, calledTools)

	for _, tool := range result[0].AllowedTools {
		if tool == "tool_c" {
			t.Error("SECURITY: tool_c was added back despite not being in the original allowlist — sticky union widened access")
		}
	}
}

// TestStickyUnionBindings_SkipsUnrestrictedBindings verifies that bindings
// whose original allowlist was empty (unrestricted = "all tools") are skipped.
// Adding tools to an unrestricted binding would turn it into a restricted
// binding, changing semantics.
func TestStickyUnionBindings_SkipsUnrestrictedBindings(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:             "mcp1",
				AvailableTools: []string{"tool_a", "tool_b", "tool_c"},
			},
		},
	}

	spawner := &Spawner{registry: registry}

	bindings := []config.MCPBinding{
		{MCPID: "mcp1", AllowedTools: []string{"tool_a", "tool_b"}},
	}
	// Original binding was unrestricted (empty AllowedTools = "all tools").
	initialBindings := []config.MCPBinding{
		{MCPID: "mcp1", AllowedTools: nil},
	}
	calledTools := map[string]bool{"tool_c": true}

	result := spawner.stickyUnionBindings(bindings, initialBindings, calledTools)

	for _, tool := range result[0].AllowedTools {
		if tool == "tool_c" {
			t.Error("tool_c was added to an unrestricted binding, turning it into a restricted binding — semantics changed")
		}
	}
	if len(result[0].AllowedTools) != 2 {
		t.Errorf("expected 2 tools (unrestricted binding untouched), got %d: %v",
			len(result[0].AllowedTools), result[0].AllowedTools)
	}
}

// TestStickyUnionBindings_NoDuplicates verifies that tools already present
// in AllowedTools are not duplicated by the sticky union.
func TestStickyUnionBindings_NoDuplicates(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID:             "mcp1",
				AvailableTools: []string{"tool_a", "tool_b"},
			},
		},
	}

	spawner := &Spawner{registry: registry}

	bindings := []config.MCPBinding{
		{MCPID: "mcp1", AllowedTools: []string{"tool_a", "tool_b"}},
	}
	initialBindings := []config.MCPBinding{
		{MCPID: "mcp1", AllowedTools: []string{"tool_a", "tool_b"}},
	}
	calledTools := map[string]bool{"tool_a": true, "tool_b": true}

	result := spawner.stickyUnionBindings(bindings, initialBindings, calledTools)

	if len(result[0].AllowedTools) != 2 {
		t.Errorf("expected 2 tools (no duplicates), got %d: %v",
			len(result[0].AllowedTools), result[0].AllowedTools)
	}
}

// TestStickyUnionBindings_EmptyCalledTools verifies the early-return path.
func TestStickyUnionBindings_EmptyCalledTools(t *testing.T) {
	registry := &config.Registry{
		Mu:   sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{"mcp1": {ID: "mcp1", AvailableTools: []string{"tool_a"}}},
	}
	spawner := &Spawner{registry: registry}

	bindings := []config.MCPBinding{
		{MCPID: "mcp1", AllowedTools: []string{"tool_a"}},
	}
	initialBindings := []config.MCPBinding{
		{MCPID: "mcp1", AllowedTools: []string{"tool_a"}},
	}
	calledTools := map[string]bool{}

	result := spawner.stickyUnionBindings(bindings, initialBindings, calledTools)
	if len(result) != 1 || len(result[0].AllowedTools) != 1 || result[0].AllowedTools[0] != "tool_a" {
		t.Errorf("expected bindings unchanged for empty calledTools, got: %v", result)
	}
}

// TestStickyUnionBindings_DeterministicOrder is a regression test for C17
// (2026-10-20): stickyUnionBindings iterated calledTools via `for tool := range`
// (Go map iteration), producing non-deterministic AllowedTools order. This
// invalidated prompt-cache prefixes because the tool list changed order on
// every call even with identical inputs.
//
// Before the fix: AllowedTools order was randomized. After the fix: called
// tools are appended in sorted (alphabetical) order, guaranteeing deterministic
// output for identical inputs.
func TestStickyUnionBindings_DeterministicOrder(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				AvailableTools: []string{
					"venmo__get_balance",
					"venmo__send_payment",
					"venmo__show_received_payment_requests",
					"venmo__show_sent_payment_requests",
				},
			},
		},
	}

	spawner := &Spawner{registry: registry}

	// Start with an empty binding so all calledTools are appended fresh.
	makeBindings := func() []config.MCPBinding {
		return []config.MCPBinding{{MCPID: "mcp1", AllowedTools: []string{}}}
	}
	initialBindings := []config.MCPBinding{
		{MCPID: "mcp1", AllowedTools: []string{
			"venmo__get_balance",
			"venmo__send_payment",
			"venmo__show_received_payment_requests",
			"venmo__show_sent_payment_requests",
		}},
	}
	// Multiple called tools — order matters for cache stability.
	calledTools := map[string]bool{
		"venmo__show_sent_payment_requests":     true,
		"venmo__get_balance":                    true,
		"venmo__show_received_payment_requests": true,
	}

	// Run many times and assert identical output every time.
	first := spawner.stickyUnionBindings(makeBindings(), initialBindings, calledTools)
	for i := 0; i < 100; i++ {
		result := spawner.stickyUnionBindings(makeBindings(), initialBindings, calledTools)
		if len(result[0].AllowedTools) != len(first[0].AllowedTools) {
			t.Fatalf("run %d: tool count differs: %d vs %d", i, len(result[0].AllowedTools), len(first[0].AllowedTools))
		}
		for j, tool := range result[0].AllowedTools {
			if tool != first[0].AllowedTools[j] {
				t.Fatalf("run %d: tool order differs at index %d: got %q, want %q (non-deterministic map iteration)",
					i, j, tool, first[0].AllowedTools[j])
			}
		}
	}

	// Verify the appended tools are in sorted order.
	// (The binding starts empty, so all tools are appended by sticky union.)
	expected := []string{
		"venmo__get_balance",
		"venmo__show_received_payment_requests",
		"venmo__show_sent_payment_requests",
	}
	if len(first[0].AllowedTools) != len(expected) {
		t.Fatalf("expected %d tools, got %d: %v", len(expected), len(first[0].AllowedTools), first[0].AllowedTools)
	}
	for i, tool := range first[0].AllowedTools {
		if tool != expected[i] {
			t.Errorf("index %d: got %q, want sorted order %q", i, tool, expected[i])
		}
	}
}
