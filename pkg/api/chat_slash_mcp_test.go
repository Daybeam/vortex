package api

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

// TestCmdMCP_ListAndToggle covers the /mcp operator UX: list the MCP pool and
// flip the level-A (MCP) and level-B (tool) toggles. Writes go through the same
// CommitOps /mcps/{id} path the web UI's update-mcp uses.
// See docs/completed/2026-10-22/MCP_TOOL_TOGGLE_DESIGN.md.
func TestCmdMCP_ListAndToggle(t *testing.T) {
	app, tmpDir := setupTestApp(t)
	defer os.RemoveAll(tmpDir)

	// Seed one MCP through the same path the web UI uses.
	mcpJSON, _ := json.Marshal(config.MCPDef{
		ID:             "test_mcp",
		URL:            "http://localhost:9/sse",
		AvailableTools: []string{"foo", "bar"},
	})
	if err := app.Registry.CommitOps(
		[]config.PatchOp{{Op: config.OpAdd, Path: "/mcps/test_mcp", Value: json.RawMessage(mcpJSON)}},
		"test", ""); err != nil {
		t.Fatalf("seed MCP: %v", err)
	}
	h := &ChatHandler{App: app}

	if _, out := h.cmdMCP(""); !strings.Contains(out, "test_mcp [on]") {
		t.Fatalf("list must show the MCP as on, got:\n%s", out)
	}

	// Level A: disable the whole MCP.
	if _, out := h.cmdMCP("off test_mcp"); !strings.Contains(out, "updated") {
		t.Fatalf("/mcp off must succeed, got: %s", out)
	}
	if m := app.Registry.GetMCP("test_mcp"); m == nil || !m.Disabled {
		t.Fatalf("MCP must be disabled after /mcp off, got %+v", m)
	}
	if _, out := h.cmdMCP(""); !strings.Contains(out, "test_mcp [OFF]") {
		t.Fatalf("list must show OFF, got:\n%s", out)
	}

	// Level B: deny one tool, then re-enable it.
	if _, out := h.cmdMCP("off-tool test_mcp foo"); !strings.Contains(out, "updated") {
		t.Fatalf("/mcp off-tool must succeed, got: %s", out)
	}
	if m := app.Registry.GetMCP("test_mcp"); m == nil || !slices.Contains(m.DisabledTools, "foo") {
		t.Fatalf("disabled_tools must contain foo, got %+v", m)
	}
	if _, out := h.cmdMCP("on-tool test_mcp foo"); !strings.Contains(out, "updated") {
		t.Fatalf("/mcp on-tool must succeed, got: %s", out)
	}
	if m := app.Registry.GetMCP("test_mcp"); m != nil && slices.Contains(m.DisabledTools, "foo") {
		t.Fatalf("foo must be re-enabled, got %+v", m.DisabledTools)
	}

	// Unknown id is reported, not silently accepted.
	if _, out := h.cmdMCP("off nope"); !strings.Contains(out, "not found") {
		t.Fatalf("unknown id must report not found, got: %s", out)
	}
	// The toggle is system-wide, so the command must be admin-gated.
	if !IsAdminCommand("/mcp") {
		t.Fatal("/mcp must be admin-gated (system-wide toggle)")
	}
}
