package core

import (
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// MCP / tool toggle (2026-10-22). See docs/completed/2026-10-22/MCP_TOOL_TOGGLE_DESIGN.md.
//
// Two operator-controlled, strictly-subtractive switches:
//   - MCPDef.Disabled      : level A — the whole MCP leaves the model's surface.
//   - MCPDef.DisabledTools : level B — individual tools (per-MCP deny list).
//
// One predicate is used by EVERY layer (discovery, routing, schema assembly,
// dispatch) so an advertised-but-refused surface cannot occur.
//
// Strictly subtractive: neither switch grants anything, loosens a tier gate, or
// bypasses MountPolicy — the pool keeps its authorization relations untouched.

// mcpActive reports whether an MCP participates in the tool surface.
// Nil-safe: an unknown MCP is inactive.
// @decision:DECISION-MCP-TOGGLE
func mcpActive(m *config.MCPDef) bool {
	return m != nil && !m.Disabled
}

// toolEnabled reports whether a tool of an MCP may be exposed or dispatched:
// the MCP must be active AND the tool name must not be in its deny list. It does
// NOT widen — callers still intersect with available_tools / AllowedTools.
func toolEnabled(m *config.MCPDef, toolName string) bool {
	return mcpActive(m) && !containsStr(m.DisabledTools, toolName)
}

// visibleTools filters an MCP's full tool definitions to the set a binding may
// expose AND the operator has not disabled. Shared by buildMCPServers and
// collectDelegationToolDefs so the two export paths cannot diverge.
func visibleTools(m *config.MCPDef, b config.MCPBinding) []schemas.ToolDefinition {
	if !mcpActive(m) {
		return nil
	}
	allowed := make(map[string]bool, len(b.AllowedTools))
	for _, t := range b.AllowedTools {
		allowed[t] = true
	}
	out := make([]schemas.ToolDefinition, 0, len(m.FullToolDefinitions))
	for _, t := range m.FullToolDefinitions {
		if b.IsRestricted() && !allowed[t.Name] {
			continue
		}
		if !toolEnabled(m, t.Name) {
			continue
		}
		out = append(out, t)
	}
	return out
}
