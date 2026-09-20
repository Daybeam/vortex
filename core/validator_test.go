package core

import (
	"github.com/daybeam/vortex/config"
	"testing"
)

func TestResolveFinalMCPs(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"mcp_git": {ID: "mcp_git"},
			"mcp_fs":  {ID: "mcp_fs"},
		},
		Roles: map[string]*config.Role{
			"role_dev": {
				ID:               "role_dev",
				AllowDynamicMCPs: true,
				BoundMCPBindings: []config.MCPBinding{
					{MCPID: "mcp_git", AllowedTools: []string{"git_commit"}},
				},
			},
			"role_strict": {
				ID:               "role_strict",
				AllowDynamicMCPs: false,
			},
		},
	}

	hub := NewContextHub(reg, nil, nil)

	// Test 1: Successful merge
	additional := []string{"mcp_fs"}
	allowlists := map[string][]string{
		"mcp_fs": {"read_file"},
	}
	bindings, err := ResolveFinalMCPs(hub, "role_dev", additional, allowlists)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(bindings) != 2 {
		t.Errorf("expected 2 bindings, got %d", len(bindings))
	}

	foundFS := false
	for _, b := range bindings {
		if b.MCPID == "mcp_fs" {
			foundFS = true
			if len(b.AllowedTools) != 1 || b.AllowedTools[0] != "read_file" {
				t.Errorf("unexpected allowlist for mcp_fs: %v", b.AllowedTools)
			}
		}
	}
	if !foundFS {
		t.Error("mcp_fs not found in bindings")
	}

	// Test 2: Dynamic MCPs not allowed
	_, err = ResolveFinalMCPs(hub, "role_strict", additional, nil)
	if err == nil {
		t.Error("expected error for dynamic MCPs on strict role, got nil")
	}

	// Test 3: Unknown MCP
	_, err = ResolveFinalMCPs(hub, "role_dev", []string{"unknown"}, nil)
	if err == nil {
		t.Error("expected error for unknown MCP, got nil")
	}
}
