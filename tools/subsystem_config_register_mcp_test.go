package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestRegisterMCP_WritesToMCPsNotRoles is a regression test for B-28-1:
// register_mcp had a copy-paste bug from register_role — it wrote to /roles/
// instead of /mcps/, and all error messages said "register_role". This test
// verifies that register_mcp creates an entry in Registry.MCPs, not
// Registry.Roles.
//
// Bug: B-28-1 (2026-10-28) — register_mcp copy-paste of register_role.
// Link: AUDIT_REPORT_2026-10-28.md §B-28-1
func TestRegisterMCP_WritesToMCPsNotRoles(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0644); err != nil {
		t.Fatalf("setup: write config: %v", err)
	}

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	app := &App{
		Registry: reg,
		Tier:     TierAdmin,
	}
	registerConfigSubsystem(app)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)

	_, err = registry.Invoke(ctx, app, "config", "register_mcp", map[string]any{
		"id":      "test-mcp-b28-1",
		"url":     "http://localhost:9999",
		"trusted": true,
	})
	if err != nil {
		t.Fatalf("register_mcp failed: %v", err)
	}

	// B-28-1 core assertion: must land in MCPs, NOT Roles.
	if reg.MCPs["test-mcp-b28-1"] == nil {
		t.Error("B-28-1: register_mcp did not add entry to Registry.MCPs")
	}
	if reg.Roles["test-mcp-b28-1"] != nil {
		t.Error("B-28-1: register_mcp incorrectly added entry to Registry.Roles (copy-paste bug from register_role)")
	}
}

// TestRegisterMCP_StdioTransport is a companion test for B-28-1: now that
// register_mcp writes to /mcps/ and declares command/args parameters, verify
// that a stdio (command-based) MCP can be registered via the tool action —
// previously the parameter schema only documented "url".
//
// Bug: B-28-1 (2026-10-28) — register_mcp only declared url parameter.
func TestRegisterMCP_StdioTransport(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0644); err != nil {
		t.Fatalf("setup: write config: %v", err)
	}

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	app := &App{
		Registry: reg,
		Tier:     TierAdmin,
	}
	registerConfigSubsystem(app)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)

	_, err = registry.Invoke(ctx, app, "config", "register_mcp", map[string]any{
		"id":      "test-stdio-mcp",
		"command": "python3",
		"args":    []any{"-m", "server"},
		"dir":     "/tmp",
		"trusted": true,
	})
	if err != nil {
		t.Fatalf("register_mcp stdio failed: %v", err)
	}

	mcp := reg.MCPs["test-stdio-mcp"]
	if mcp == nil {
		t.Fatal("B-28-1: stdio MCP not registered in Registry.MCPs")
	}
	if mcp.Command != "python3" {
		t.Errorf("B-28-1: expected Command='python3', got %q", mcp.Command)
	}
}

// TestUnregisterMCP_RemovesFromMCPs is a regression test for B-28-3:
// there was no unregister_mcp tool action (unlike unregister_role and
// unregister_group), making register_mcp irreversible via the tool layer.
// This test verifies that unregister_mcp removes the MCP from Registry.MCPs.
//
// Bug: B-28-3 (2026-10-28) — no unregister_mcp tool action existed.
func TestUnregisterMCP_RemovesFromMCPs(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{}`), 0644); err != nil {
		t.Fatalf("setup: write config: %v", err)
	}

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	app := &App{
		Registry: reg,
		Tier:     TierAdmin,
	}
	registerConfigSubsystem(app)

	ctx := context.WithValue(context.Background(), AuthTierKey, TierAdmin)

	// Register an MCP first.
	_, err = registry.Invoke(ctx, app, "config", "register_mcp", map[string]any{
		"id":      "test-unreg-mcp",
		"url":     "http://localhost:9999",
		"trusted": true,
	})
	if err != nil {
		t.Fatalf("register_mcp failed: %v", err)
	}
	if reg.MCPs["test-unreg-mcp"] == nil {
		t.Fatal("setup: MCP not registered")
	}

	// B-28-3: unregister_mcp must remove it.
	_, err = registry.Invoke(ctx, app, "config", "unregister_mcp", map[string]any{
		"id": "test-unreg-mcp",
	})
	if err != nil {
		t.Fatalf("unregister_mcp failed: %v", err)
	}

	if reg.MCPs["test-unreg-mcp"] != nil {
		t.Error("B-28-3: unregister_mcp did not remove entry from Registry.MCPs")
	}
}
