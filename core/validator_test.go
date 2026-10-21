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
	bindings, err := ResolveFinalMCPs(hub, "role_dev", additional, allowlists, true)
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

	// Test 2: Dynamic MCPs blocked for non-admin callers (tier check, not role flag)
	_, err = ResolveFinalMCPs(hub, "role_strict", additional, nil, false)
	if err == nil {
		t.Error("expected error for non-admin caller mounting dynamic MCPs, got nil")
	}

	// Test 3: Unknown MCP
	_, err = ResolveFinalMCPs(hub, "role_dev", []string{"unknown"}, nil, true)
	if err == nil {
		t.Error("expected error for unknown MCP, got nil")
	}
}
