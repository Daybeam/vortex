package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestGenerateDynamicMenu_WithRolesAndMCPs(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{
			"market_researcher": {
				Name:    "Market Researcher",
				Purpose: "Financial market and macro research",
			},
			"code_reviewer": {
				Name:    "Code Reviewer",
				Purpose: "Review and refactor code",
			},
		},
		MCPs: map[string]*config.MCPDef{
			"invest-research-lite": {
				ID:             "invest-research-lite",
				AvailableTools: make([]string, 77),
				Domain:         "research",
			},
			"fred": {
				ID:     "fred",
				Domain: "research",
			},
			"pyright": {
				ID:     "pyright",
				Domain: "coding",
			},
		},
	}

	menu := (&CapabilityNavigator{Registry: reg}).GenerateDynamicMenu(reg)

	// Must contain roles
	if !strings.Contains(menu, "Market Researcher") {
		t.Error("menu missing role 'Market Researcher'")
	}
	if !strings.Contains(menu, "Code Reviewer") {
		t.Error("menu missing role 'Code Reviewer'")
	}

	// Must contain MCPs grouped by domain
	if !strings.Contains(menu, "**research**") {
		t.Error("menu missing 'research' domain section")
	}
	if !strings.Contains(menu, "invest-research-lite") {
		t.Error("menu missing invest-research-lite")
	}
	if !strings.Contains(menu, "**coding**") {
		t.Error("menu missing 'coding' domain section")
	}
	if !strings.Contains(menu, "pyright") {
		t.Error("menu missing pyright")
	}
}

func TestGenerateDynamicMenu_EmptyRegistry(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{},
		MCPs:  map[string]*config.MCPDef{},
	}

	menu := (&CapabilityNavigator{Registry: reg}).GenerateDynamicMenu(reg)

	// Empty registry should still return a valid header
	if !strings.Contains(menu, "Dynamic Capability Menu") {
		t.Error("empty registry should still show header")
	}
}

func TestGenerateDynamicMenu_UnassignedDomain(t *testing.T) {
	reg := &config.Registry{
		Roles: map[string]*config.Role{},
		MCPs: map[string]*config.MCPDef{
			"mystery-mcp": {ID: "mystery-mcp"}, // No domain set
		},
	}

	menu := (&CapabilityNavigator{Registry: reg}).GenerateDynamicMenu(reg)

	if !strings.Contains(menu, "**General**") {
		t.Error("MCP without domain should be grouped under 'General'")
	}
}
