package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestFormatRoleDetail_ShowsBoundMCPsAndTools(t *testing.T) {
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
		MCPs:  make(map[string]*config.MCPDef),
	}

	reg.Roles["test_coder"] = &config.Role{
		ID:             "test_coder",
		Name:           "Test Coder",
		BaseCapability: "code.implement",
		Purpose:        "Writes and tests code",
		BoundSkills:    []string{"code_review"},
		BoundMCPBindings: []config.MCPBinding{
			{MCPID: "gitnexus", AllowedTools: []string{}},
			{MCPID: "pyright", AllowedTools: []string{"check", "fix"}},
			{MCPID: "notloaded", AllowedTools: []string{}},
		},
	}

	reg.MCPs["gitnexus"] = &config.MCPDef{
		ID:             "gitnexus",
		AvailableTools: []string{"search", "impact", "references"},
		FullToolDefinitions: []schemas.ToolDefinition{
			{Name: "search", Description: "Search code symbols"},
			{Name: "impact", Description: "Analyze change impact"},
			{Name: "references", Description: "Find references"},
		},
	}

	reg.MCPs["pyright"] = &config.MCPDef{
		ID:             "pyright",
		AvailableTools: []string{"check", "fix", "hover"},
		FullToolDefinitions: []schemas.ToolDefinition{
			{Name: "check", Description: "Type check Python"},
			{Name: "fix", Description: "Auto-fix type errors"},
			{Name: "hover", Description: "Hover info"},
		},
	}

	nav := NewCapabilityNavigator(reg)
	output := nav.ExploreIntent("test_coder")

	if !strings.Contains(output, "### Bound MCP Servers & Tools") {
		t.Error("output should contain 'Bound MCP Servers & Tools' section")
	}
	if !strings.Contains(output, "gitnexus") {
		t.Error("output should mention gitnexus")
	}
	if !strings.Contains(output, "unrestricted") {
		t.Error("output should label gitnexus as unrestricted")
	}
	if !strings.Contains(output, "`search` - Search code symbols") {
		t.Error("output should show gitnexus search tool with description")
	}
	if !strings.Contains(output, "`impact` - Analyze change impact") {
		t.Error("output should show gitnexus impact tool with description")
	}
	if !strings.Contains(output, "pyright") {
		t.Error("output should mention pyright")
	}
	if !strings.Contains(output, "restricted to 2 tools") {
		t.Error("output should label pyright as restricted to 2 tools")
	}
	if !strings.Contains(output, "`check` - Type check Python") {
		t.Error("output should show pyright check tool with description")
	}
	if !strings.Contains(output, "`fix` - Auto-fix type errors") {
		t.Error("output should show pyright fix tool with description")
	}
	if strings.Contains(output, "`hover`") {
		t.Error("output should NOT show pyright hover tool (not in allowed list)")
	}
	if !strings.Contains(output, "notloaded") {
		t.Error("output should mention notloaded MCP")
	}
	if !strings.Contains(output, "not loaded") {
		t.Error("output should indicate notloaded is not loaded")
	}
}

func TestFormatRoleDetail_NoMCPBindings(t *testing.T) {
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
		MCPs:  make(map[string]*config.MCPDef),
	}

	reg.Roles["simple_role"] = &config.Role{
		ID:             "simple_role",
		Name:           "Simple Role",
		BaseCapability: "general",
		BoundSkills:    []string{"basic"},
	}

	nav := NewCapabilityNavigator(reg)
	output := nav.ExploreIntent("simple_role")

	if strings.Contains(output, "Bound MCP Servers") {
		t.Error("output should not contain MCP section when role has no bindings")
	}
}
