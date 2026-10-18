package core

import (
	"context"
	"sync"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestRoute_AlwaysInclude verifies that tools listed in AlwaysInclude
// are kept in the Route output even when they don't match isRelevant
// (C1-② regression test).
func TestRoute_AlwaysInclude(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				AvailableTools: []string{
					"venmo__show_profile",
					"venmo__send_payment",
					"supervisor__complete_task",
					"supervisor__show_profile",
				},
			},
		},
	}

	router := NewToolRouter(registry, nil)

	bindings := []config.MCPBinding{
		{
			MCPID: "mcp1",
			AlwaysInclude: []string{
				"supervisor__complete_task",
				"supervisor__show_profile",
			},
		},
	}

	req := RouteRequest{
		Task:     "send a venmo payment to my friend",
		Bindings: bindings,
	}

	routed := router.Route(context.Background(), req)
	if len(routed) == 0 {
		t.Fatal("expected at least one routed binding")
	}

	allowed := make(map[string]bool)
	for _, tool := range routed[0].AllowedTools {
		allowed[tool] = true
	}

	for _, must := range []string{"supervisor__complete_task", "supervisor__show_profile"} {
		if !allowed[must] {
			t.Errorf("always_include tool %q missing from routed output", must)
		}
	}
}

// TestRoute_AlwaysIncludeWithRestrictedBinding verifies that AlwaysInclude
// works even when the binding has AllowedTools set (restricted mode).
func TestRoute_AlwaysIncludeWithRestrictedBinding(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				AvailableTools: []string{
					"tool_a",
					"tool_b",
					"tool_c",
				},
			},
		},
	}

	router := NewToolRouter(registry, nil)

	bindings := []config.MCPBinding{
		{
			MCPID:         "mcp1",
			AllowedTools:  []string{"tool_a", "tool_b", "tool_c"},
			AlwaysInclude: []string{"tool_c"},
		},
	}

	req := RouteRequest{
		Task:     "do something with tool_a",
		Bindings: bindings,
	}

	routed := router.Route(context.Background(), req)
	if len(routed) == 0 {
		t.Fatal("expected at least one routed binding")
	}

	allowed := make(map[string]bool)
	for _, tool := range routed[0].AllowedTools {
		allowed[tool] = true
	}

	if !allowed["tool_c"] {
		t.Error("always_include tool_c missing from restricted binding output")
	}
}
