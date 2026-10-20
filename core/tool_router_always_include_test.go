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

// TestRoute_AlwaysInclude_DropsToolMissingFromCatalog pins the phantom-tool
// guard: a name in always_include that the MCP server does not serve must not
// reach the model's surface.
//
// Reproduces AppWorld 4fab96f_3 (run u, 2026-10-19) in miniature: the role
// asked for phone__get_current_date_and_time, the app whitelist loaded only
// venmo+supervisor, and the routed surface carried the phone tool anyway. The
// agent called it 5 times and got an empty body every time.
func TestRoute_AlwaysInclude_DropsToolMissingFromCatalog(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"appworld": {
				ID: "appworld",
				AvailableTools: []string{
					"venmo__login",
					"venmo__remind_payment_request",
					"supervisor__complete_task",
				},
			},
		},
	}
	router := NewToolRouter(registry, nil)

	bindings := []config.MCPBinding{{
		MCPID: "appworld",
		AlwaysInclude: []string{
			"supervisor__complete_task",
			"phone__get_current_date_and_time", // not in catalog
		},
	}}

	routed := router.Route(context.Background(), RouteRequest{
		Task:     "remind my roommates about unpaid requests",
		Bindings: bindings,
	})
	if len(routed) == 0 {
		t.Fatal("expected one routed binding")
	}

	allowed := make(map[string]bool)
	for _, tool := range routed[0].AllowedTools {
		allowed[tool] = true
	}
	if !allowed["supervisor__complete_task"] {
		t.Error("legitimate always_include entry was dropped")
	}
	if allowed["phone__get_current_date_and_time"] {
		t.Error("always_include resurrected a tool absent from the MCP catalog; " +
			"the agent can call it but every call returns an empty body")
	}
}

// TestRoute_AlwaysInclude_KeepsEntriesWhenCatalogIsEmpty is the counterweight.
// AvailableTools is empty until discovery runs, so a membership check must not
// fire on an empty catalog — that would delete the safety net exactly during
// the cold-start window it exists to cover.
func TestRoute_AlwaysInclude_KeepsEntriesWhenCatalogIsEmpty(t *testing.T) {
	registry := &config.Registry{
		Mu:  sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{"appworld": {ID: "appworld"}},
	}
	router := NewToolRouter(registry, nil)

	bindings := []config.MCPBinding{{
		MCPID:         "appworld",
		AlwaysInclude: []string{"supervisor__complete_task", "supervisor__show_profile"},
	}}

	routed := router.Route(context.Background(), RouteRequest{
		Task:     "remind my roommates",
		Bindings: bindings,
	})
	if len(routed) == 0 {
		t.Fatal("expected one routed binding")
	}

	allowed := make(map[string]bool)
	for _, tool := range routed[0].AllowedTools {
		allowed[tool] = true
	}
	for _, must := range []string{"supervisor__complete_task", "supervisor__show_profile"} {
		if !allowed[must] {
			t.Errorf("always_include %q dropped on an undiscovered (empty) catalog; "+
				"the cold-start safety net must survive", must)
		}
	}
}
