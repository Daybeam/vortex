package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ToolBindingWarning is emitted when a role references a tool that does not
// exist in any MCP's AvailableTools. The loader logs each warning at startup.
// This is a pure validation — zero behavior change, no runtime effect.
type ToolBindingWarning struct {
	RoleID    string
	MCPID     string
	Tool      string
	Category  string // "binding_mcp_missing", "binding_tool_missing", "instruction_ghost"
	Detail    string
}

func (w ToolBindingWarning) String() string {
	switch w.Category {
	case "binding_mcp_missing":
		return fmt.Sprintf("role %q: binding references MCP %q which is not registered", w.RoleID, w.MCPID)
	case "binding_tool_missing":
		return fmt.Sprintf("role %q: binding MCP %q AllowedTools lists %q but MCP.AvailableTools does not contain it", w.RoleID, w.MCPID, w.Tool)
	case "instruction_ghost":
		return fmt.Sprintf("role %q: instruction/rules reference tool %q but no registered MCP provides it — likely a ghost tool name", w.RoleID, w.Tool)
	default:
		return fmt.Sprintf("role %q: %s", w.RoleID, w.Detail)
	}
}

// backtickToolRe extracts backtick-quoted tokens that look like tool names.
// Tool names in this codebase are snake_case (e.g. get_reservation_details,
// find_user_id_by_name_zip). We require at least one underscore and length >= 5
// to reduce false positives on non-tool tokens like `ok` or `config`.
var backtickToolRe = regexp.MustCompile("`([a-z][a-z0-9_]{4,})`")

// bareToolRe matches unquoted snake_case tokens starting with a common tool
// verb prefix. This catches tool names listed without backticks (e.g. the
// §8.33 pattern: "e.g. get_reservation_details, find_user_id_by_name_zip").
// The verb prefix requirement keeps false positives low — field names like
// user_id or reservation_id don't start with verbs.
var bareToolRe = regexp.MustCompile(`\b((?:get|set|update|create|delete|find|search|list|send|transfer|book|cancel|validate|check|read|write|execute)_[a-z0-9_]{3,})\b`)

// nonToolTokens are snake_case words that look like tools but aren't.
var nonToolTokens = map[string]bool{
	"exit_code": true, "error_code": true, "status_code": true,
	"file_path": true, "output_dir": true, "config_file": true,
	"log_level": true, "base_url": true, "auth_token": true,
	"read_only": true, "write_only": true, "check_in": true,
	"check_out": true, "set_up": true,
}

// isToolLike returns true if a token looks like a tool name and is not a
// known non-tool word.
func isToolLike(token string) bool {
	if !strings.Contains(token, "_") {
		return false
	}
	return !nonToolTokens[token]
}

// ValidateRoleToolBindings checks every role's BoundMCPBindings against the
// registered MCPs and scans instruction/rules text for ghost tool names.
// Returns warnings (never errors — this is advisory, zero behavior change).
//
// Two checks:
//  1. Structural: binding.AllowedTools ⊆ mcp.AvailableTools
//  2. Textual: backtick-quoted tool-like tokens in Instruction/Rules must
//     exist in at least one registered MCP's AvailableTools
//
// If an MCP has no AvailableTools declared (empty — tools discovered at
// runtime), the structural check is skipped for that binding (can't validate
// against an unknown set). The textual check uses the union of all MCPs that
// DO declare AvailableTools; if none declare any, it is skipped entirely.
func ValidateRoleToolBindings(roles map[string]*Role, mcps map[string]*MCPDef) []ToolBindingWarning {
	if len(roles) == 0 || len(mcps) == 0 {
		return nil
	}

	// Build a global tool set from all MCPs that declare AvailableTools.
	globalTools := make(map[string]bool)
	anyToolsDeclared := false
	for _, mcp := range mcps {
		if len(mcp.AvailableTools) > 0 {
			anyToolsDeclared = true
			for _, t := range mcp.AvailableTools {
				globalTools[t] = true
			}
		}
	}

	var warnings []ToolBindingWarning

	for _, role := range roles {
		if role == nil {
			continue
		}

		// --- Structural check: binding.AllowedTools ⊆ mcp.AvailableTools ---
		for _, b := range role.BoundMCPBindings {
			mcp, ok := mcps[b.MCPID]
			if !ok {
				warnings = append(warnings, ToolBindingWarning{
					RoleID:   role.ID,
					MCPID:    b.MCPID,
					Category: "binding_mcp_missing",
				})
				continue
			}
			// Skip if MCP hasn't declared AvailableTools (runtime discovery).
			if len(mcp.AvailableTools) == 0 {
				continue
			}
			mcpToolSet := make(map[string]bool, len(mcp.AvailableTools))
			for _, t := range mcp.AvailableTools {
				mcpToolSet[t] = true
			}
			for _, tool := range b.AllowedTools {
				if !mcpToolSet[tool] {
					warnings = append(warnings, ToolBindingWarning{
						RoleID:   role.ID,
						MCPID:    b.MCPID,
						Tool:     tool,
						Category: "binding_tool_missing",
					})
				}
			}
		}

		// --- Textual check: ghost tool names in instruction/rules ---
		// Only run if at least one MCP declares AvailableTools.
		if !anyToolsDeclared {
			continue
		}
		for _, text := range []string{role.Instruction, role.Rules} {
			if text == "" {
				continue
			}
			// Collect candidate tool names from both backtick-quoted and
			// bare verb-prefixed tokens. Deduplicate via a set.
			candidates := make(map[string]bool)
			for _, match := range backtickToolRe.FindAllStringSubmatch(text, -1) {
				if isToolLike(match[1]) {
					candidates[match[1]] = true
				}
			}
			for _, match := range bareToolRe.FindAllStringSubmatch(text, -1) {
				if isToolLike(match[1]) {
					candidates[match[1]] = true
				}
			}
			for token := range candidates {
				if !globalTools[token] {
					warnings = append(warnings, ToolBindingWarning{
						RoleID:   role.ID,
						Tool:     token,
						Category: "instruction_ghost",
					})
				}
			}
		}
	}

	return warnings
}
