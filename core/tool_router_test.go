/*
 * ToolRouter SearchTools — keyword-based tool search with pagination.
 */

package core

import (
	"context"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"sync"
	"testing"
)

func TestSearchTools(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "get_earnings", Description: "Get earnings data"},
					{Name: "earnings_report", Description: "Get earnings report"},
				},
			},
		},
	}

	router := NewToolRouter(registry, nil)

	// Test 1: Successful search (roleID="" → search all MCPs)
	res, err := router.SearchTools(context.Background(), "earnings", "", "", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(res.Tools))
	}

	// Test 2: Pagination
	res, err = router.SearchTools(context.Background(), "earnings", "", "", 1, 1)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(res.Tools))
	}

	// Test 3: Zero results
	res, err = router.SearchTools(context.Background(), "unknown", "", "", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(res.Tools))
	}
}

// D3 fix (2026-10-20): SearchTools with roleID must scope to the role's bound
// MCPs only. A role with no bindings gets empty results (no pool fallback).
// See CUSTOMER_V010_AUDIT_2026-10-20.md §D3.
func TestSearchTools_RoleScoped(t *testing.T) {
	registry := &config.Registry{
		Mu: sync.RWMutex{},
		MCPs: map[string]*config.MCPDef{
			"mcp1": {
				ID: "mcp1",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "get_earnings", Description: "Get earnings data"},
				},
			},
			"mcp2": {
				ID: "mcp2",
				FullToolDefinitions: []schemas.ToolDefinition{
					{Name: "send_email", Description: "Send an email"},
				},
			},
		},
		Roles: map[string]*config.Role{
			"role_with_mcp1": {
				ID:               "role_with_mcp1",
				BoundMCPBindings: []config.MCPBinding{{MCPID: "mcp1"}},
			},
			"role_no_bindings": {
				ID: "role_no_bindings",
			},
		},
	}

	router := NewToolRouter(registry, nil)

	// Role bound to mcp1 → can search mcp1's tools, NOT mcp2's.
	res, err := router.SearchTools(context.Background(), "", "", "role_with_mcp1", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "get_earnings" {
		t.Fatalf("expected only get_earnings from mcp1, got %v", res.Tools)
	}

	// Role bound to mcp1 → cannot find mcp2's tools.
	res, err = router.SearchTools(context.Background(), "email", "", "role_with_mcp1", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("expected 0 tools (email is in mcp2, not bound to role), got %v", res.Tools)
	}

	// Role with no bindings → empty results (no pool fallback).
	res, err = router.SearchTools(context.Background(), "", "", "role_no_bindings", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("expected 0 tools for role with no bindings, got %d", len(res.Tools))
	}

	// Non-existent role → empty results.
	res, err = router.SearchTools(context.Background(), "", "", "nonexistent", 1, 10)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(res.Tools) != 0 {
		t.Fatalf("expected 0 tools for nonexistent role, got %d", len(res.Tools))
	}
}
