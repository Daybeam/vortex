package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestRoute_LazyDiscoveryPreservesSelectionAndSafetyOrdering covers the
// per-binding orchestration extracted for O14: discovery must refresh the
// catalog before allowlist selection, privileged pruning, and always_include.
func TestRoute_LazyDiscoveryPreservesSelectionAndSafetyOrdering(t *testing.T) {
	reg := &config.Registry{MCPs: map[string]*config.MCPDef{
		"cold": {ID: "cold", Command: "server"},
	}}
	router := NewToolRouter(reg, nil)
	var gotMCP, gotTask string
	router.SetLazyDiscoveryHook(func(_ context.Context, mcpID, taskID string) error {
		gotMCP, gotTask = mcpID, taskID
		reg.MCPs["cold"].AvailableTools = []string{
			"read_file", "write_file", "orchestrator_submit_decision", "supervisor__complete_task",
		}
		return errors.New("discovered with a warning")
	})

	routed := router.Route(context.Background(), RouteRequest{
		Task:   "read the file",
		TaskID: "task-14",
		Bindings: []config.MCPBinding{{
			MCPID:        "cold",
			AllowedTools: []string{"read_file", "orchestrator_submit_decision", "supervisor__complete_task"},
			AlwaysInclude: []string{
				"supervisor__complete_task", "orchestrator_submit_decision", "write_file",
			},
		}},
	})
	if gotMCP != "cold" || gotTask != "task-14" {
		t.Fatalf("lazy discovery got mcp=%q task=%q", gotMCP, gotTask)
	}
	if len(routed) != 1 {
		t.Fatalf("got %d routed bindings, want 1", len(routed))
	}
	want := []string{"read_file", "supervisor__complete_task"}
	if !reflect.DeepEqual(routed[0].AllowedTools, want) {
		t.Fatalf("routed tools = %v, want %v", routed[0].AllowedTools, want)
	}
}

// TestRoute_FallbackHonorsExhaustedSharedBudget covers the extracted fallback
// path: exploration is a safety net, not a way around a prior binding's budget.
func TestRoute_FallbackHonorsExhaustedSharedBudget(t *testing.T) {
	reg := &config.Registry{MCPs: map[string]*config.MCPDef{
		"first": {
			ID:             "first",
			AvailableTools: []string{"read_file"},
			FullToolDefinitions: []schemas.ToolDefinition{
				toolDef("read_file", 100),
			},
		},
		"second": {
			ID:             "second",
			AvailableTools: []string{"write_file", "list_files"},
		},
	}}
	router := NewToolRouter(reg, nil)
	routed := router.Route(context.Background(), RouteRequest{
		Task:           "read the file",
		MaxTokenBudget: 100,
		Bindings: []config.MCPBinding{
			{MCPID: "first"},
			{MCPID: "second"},
		},
	})
	if len(routed) != 2 {
		t.Fatalf("got %d routed bindings, want 2", len(routed))
	}
	if got := routed[0].AllowedTools; !reflect.DeepEqual(got, []string{"read_file"}) {
		t.Fatalf("first binding = %v", got)
	}
	if got := routed[1].AllowedTools; len(got) != 0 {
		t.Fatalf("fallback bypassed exhausted shared budget: %v", got)
	}
}
