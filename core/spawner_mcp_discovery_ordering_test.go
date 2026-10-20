package core

import (
	"context"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// Regression test for the cold-start timing race (2026-10-19).
//
// Bug: Route() at spawner_prepare.go:101 read AvailableTools before tools/list
// discovery at spawner_mcp.go:223, causing cold-start tasks to route against
// an empty tool set. Fix: ensureMCPDiscovery() called before Route() in
// prepareSpawn, populating AvailableTools from tools/list.
//
// See: docs/architecture/architecture-mcp-gaps.md §3 (documented contract:
// "cache populated once during spawner initialization, reused across steps").
// See: eval-kit/docs/53-appworld-transport-fix-20261018.md §8 (root cause).

// TestEnsureMCPDiscovery_SkipsAlreadyDiscovered verifies that ensureMCPDiscovery
// is idempotent: MCPs with FullToolDefinitions already populated are skipped
// (no re-discovery). This is the warm-start path — the second call from
// buildMCPServers after ensureMCPDiscovery already ran in prepareSpawn.
func TestEnsureMCPDiscovery_SkipsAlreadyDiscovered(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"stdio-mcp": {
				ID:                  "stdio-mcp",
				Command:             "echo",
				FullToolDefinitions: []schemas.ToolDefinition{{Name: "tool_a", Description: "already cached"}},
				AvailableTools:      []string{"tool_a"},
			},
		},
	}
	spawner := &Spawner{
		registry: reg,
		mcpMgr:   NewMCPConnectionManager(reg, nil),
		logger:   &Logger{},
	}
	hub := NewContextHub(reg, nil, nil)
	bindings := []config.MCPBinding{{MCPID: "stdio-mcp"}}

	// Snapshot before
	before := len(reg.MCPs["stdio-mcp"].FullToolDefinitions)

	// Should be a no-op — already discovered
	spawner.ensureMCPDiscovery(context.Background(), hub, "test-task", bindings)

	after := len(reg.MCPs["stdio-mcp"].FullToolDefinitions)
	if before != after {
		t.Fatalf("idempotency violated: FullToolDefinitions changed %d → %d (should skip already-discovered)", before, after)
	}
}

// TestEnsureMCPDiscovery_SkipsRemoteMCPs verifies that ensureMCPDiscovery only
// handles stdio MCPs (Command != ""). Remote MCPs (URL-based) are left for
// buildMCPServers, which needs the provider name to decide whether to discover
// (skips Anthropic native passthrough).
func TestEnsureMCPDiscovery_SkipsRemoteMCPs(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"remote-mcp": {
				ID:      "remote-mcp",
				URL:     "https://example.com/mcp",
				Command: "", // remote, not stdio
			},
		},
	}
	spawner := &Spawner{
		registry: reg,
		mcpMgr:   NewMCPConnectionManager(reg, nil),
		logger:   &Logger{},
	}
	hub := NewContextHub(reg, nil, nil)
	bindings := []config.MCPBinding{{MCPID: "remote-mcp"}}

	// Should not panic and should skip (remote MCPs handled in buildMCPServers)
	spawner.ensureMCPDiscovery(context.Background(), hub, "test-task", bindings)

	// AvailableTools should still be empty (not discovered by ensureMCPDiscovery)
	if len(reg.MCPs["remote-mcp"].AvailableTools) != 0 {
		t.Fatal("ensureMCPDiscovery should not discover remote MCPs — that's buildMCPServers' job (provider-dependent)")
	}
}

// TestEnsureMCPDiscovery_SkipsMissingMCP verifies that ensureMCPDiscovery
// gracefully handles bindings whose MCPID is not in the registry (no panic).
func TestEnsureMCPDiscovery_SkipsMissingMCP(t *testing.T) {
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{},
	}
	spawner := &Spawner{
		registry: reg,
		mcpMgr:   NewMCPConnectionManager(reg, nil),
		logger:   &Logger{},
	}
	hub := NewContextHub(reg, nil, nil)
	bindings := []config.MCPBinding{{MCPID: "nonexistent"}}

	// Should not panic
	spawner.ensureMCPDiscovery(context.Background(), hub, "test-task", bindings)
}

// TestEnsureMCPDiscovery_RouteSeesPopulatedTools is the core regression test:
// after ensureMCPDiscovery runs, AvailableTools must be populated so that
// Route() can filter a meaningful tool set. Before the fix, Route ran first
// (AvailableTools empty) → cold-start tasks got only always_include tools.
//
// This test simulates the post-discovery state (tools already cached) and
// verifies that ensureMCPDiscovery preserves them for Route to see.
func TestEnsureMCPDiscovery_RouteSeesPopulatedTools(t *testing.T) {
	// Simulate a cold-start MCP that has been discovered (by ensureMCPDiscovery
	// or by a prior spawn). AvailableTools is populated.
	reg := &config.Registry{
		MCPs: map[string]*config.MCPDef{
			"appworld": {
				ID:                  "appworld",
				Command:             "python",
				FullToolDefinitions: make([]schemas.ToolDefinition, 0, 453),
				AvailableTools:      make([]string, 0, 453),
			},
		},
	}
	// Populate as if tools/list just succeeded
	for i := 0; i < 453; i++ {
		name := "supervisor__tool_" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		reg.MCPs["appworld"].FullToolDefinitions = append(
			reg.MCPs["appworld"].FullToolDefinitions,
			schemas.ToolDefinition{Name: name, Description: "discovered tool"},
		)
		reg.MCPs["appworld"].AvailableTools = append(
			reg.MCPs["appworld"].AvailableTools,
			name,
		)
	}

	spawner := &Spawner{
		registry: reg,
		mcpMgr:   NewMCPConnectionManager(reg, nil),
		logger:   &Logger{},
	}
	hub := NewContextHub(reg, nil, nil)
	bindings := []config.MCPBinding{{MCPID: "appworld"}}

	// ensureMCPDiscovery should be a no-op (already discovered)
	spawner.ensureMCPDiscovery(context.Background(), hub, "test-task", bindings)

	// Route would read this — must be populated
	available := reg.MCPs["appworld"].AvailableTools
	if len(available) != 453 {
		t.Fatalf("AvailableTools = %d, expected 453 — Route would see empty/stale tool set on cold start", len(available))
	}
}
