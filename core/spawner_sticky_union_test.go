package core

import (
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestStickyUnionBindings_AddsCalledToolsBack verifies that tools called in
// prior turns are added back to the bindings after re-routing.
//
// Before the fix: re-routing at each turn could drop tools the model was
// actively using (e.g. show_sent_payment_requests called in turn 1, dropped
// in turn 2 because the task text changed), breaking multi-turn workflows.
//
// After the fix: the sticky union adds called tools back to the bindings.
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

	// Simulate re-routed bindings that dropped a previously-called tool.
	// The model called show_sent_payment_requests in turn 1, but re-routing
	// in turn 2 only kept show_received_payment_requests and send_payment.
	bindings := []config.MCPBinding{
		{
			MCPID:        "mcp1",
			AllowedTools: []string{"venmo__show_received_payment_requests", "venmo__send_payment"},
		},
	}

	calledTools := map[string]bool{
		"venmo__show_sent_payment_requests": true, // called in turn 1, dropped by re-route
	}

	result := spawner.stickyUnionBindings(bindings, calledTools)

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

// TestStickyUnionBindings_DoesNotWidenAccess verifies that the sticky union
// never adds tools that are not in the MCP's AvailableTools. This is the
// safety guarantee: sticky union only restores what was already exposed.
func TestStickyUnionBindings_DoesNotWidenAccess(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				AvailableTools: []string{
					"tool_a",
					"tool_b",
				},
			},
		},
	}

	spawner := &Spawner{registry: registry}

	bindings := []config.MCPBinding{
		{
			MCPID:        "mcp1",
			AllowedTools: []string{"tool_a"},
		},
	}

	// tool_c is NOT in AvailableTools — it must not be added.
	calledTools := map[string]bool{
		"tool_c": true,
	}

	result := spawner.stickyUnionBindings(bindings, calledTools)

	for _, tool := range result[0].AllowedTools {
		if tool == "tool_c" {
			t.Error("sticky union widened access: tool_c is not in AvailableTools but was added")
		}
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
		{
			MCPID:        "mcp1",
			AllowedTools: []string{"tool_a", "tool_b"},
		},
	}

	calledTools := map[string]bool{
		"tool_a": true,
		"tool_b": true,
	}

	result := spawner.stickyUnionBindings(bindings, calledTools)

	if len(result[0].AllowedTools) != 2 {
		t.Errorf("expected 2 tools (no duplicates), got %d: %v",
			len(result[0].AllowedTools), result[0].AllowedTools)
	}
}

// TestStickyUnionBindings_EmptyCalledTools verifies the early-return path:
// when calledTools is empty, bindings are returned unchanged (no allocation,
// no iteration).
func TestStickyUnionBindings_EmptyCalledTools(t *testing.T) {
	registry := &config.Registry{
		Mu:    sync.RWMutex{},
		MCPs:  map[string]*config.MCPDef{"mcp1": {ID: "mcp1", AvailableTools: []string{"tool_a"}}},
	}
	spawner := &Spawner{registry: registry}

	bindings := []config.MCPBinding{
		{MCPID: "mcp1", AllowedTools: []string{"tool_a"}},
	}
	calledTools := map[string]bool{} // empty

	result := spawner.stickyUnionBindings(bindings, calledTools)
	if len(result) != 1 || len(result[0].AllowedTools) != 1 || result[0].AllowedTools[0] != "tool_a" {
		t.Errorf("expected bindings unchanged for empty calledTools, got: %v", result)
	}
}

// TestStickyUnionBindings_NilMCPDef verifies the continue path: when the
// registry returns nil for an MCP ID (e.g. unregistered MCP), that binding
// is skipped without panic and its AllowedTools are preserved.
func TestStickyUnionBindings_NilMCPDef(t *testing.T) {
	registry := &config.Registry{
		Mu:   sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{}, // no MCPs registered
	}
	spawner := &Spawner{registry: registry}

	bindings := []config.MCPBinding{
		{MCPID: "unknown_mcp", AllowedTools: []string{"tool_a"}},
	}
	calledTools := map[string]bool{"tool_a": true}

	result := spawner.stickyUnionBindings(bindings, calledTools)
	// MCP not found → binding skipped, AllowedTools unchanged
	if len(result[0].AllowedTools) != 1 || result[0].AllowedTools[0] != "tool_a" {
		t.Errorf("expected bindings unchanged when MCP not found, got: %v", result[0].AllowedTools)
	}
}

// TestStickyUnionBindings_EmptyBindings verifies that an empty bindings
// slice is handled without panic.
func TestStickyUnionBindings_EmptyBindings(t *testing.T) {
	registry := &config.Registry{
		Mu:   sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{"mcp1": {ID: "mcp1", AvailableTools: []string{"tool_a"}}},
	}
	spawner := &Spawner{registry: registry}

	calledTools := map[string]bool{"tool_a": true}
	result := spawner.stickyUnionBindings([]config.MCPBinding{}, calledTools)
	if len(result) != 0 {
		t.Errorf("expected 0 bindings for empty input, got %d", len(result))
	}
}
