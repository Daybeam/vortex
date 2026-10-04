package config

import (
	"testing"
)

func TestValidateRoleToolBindings_NoGhost(t *testing.T) {
	roles := map[string]*Role{
		"r1": {
			ID: "r1",
			BoundMCPBindings: []MCPBinding{
				{MCPID: "mcp1", AllowedTools: []string{"get_user", "update_user"}},
			},
			Instruction: "Use `get_user` and `update_user` to manage users.",
		},
	}
	mcps := map[string]*MCPDef{
		"mcp1": {ID: "mcp1", AvailableTools: []string{"get_user", "update_user", "delete_user"}},
	}
	warnings := ValidateRoleToolBindings(roles, mcps)
	if len(warnings) != 0 {
		t.Fatalf("expected 0 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestValidateRoleToolBindings_BindingMCPMissing(t *testing.T) {
	roles := map[string]*Role{
		"r1": {
			ID: "r1",
			BoundMCPBindings: []MCPBinding{
				{MCPID: "nonexistent_mcp", AllowedTools: []string{}},
			},
		},
	}
	mcps := map[string]*MCPDef{
		"mcp1": {ID: "mcp1", AvailableTools: []string{"tool_a"}},
	}
	warnings := ValidateRoleToolBindings(roles, mcps)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if warnings[0].Category != "binding_mcp_missing" {
		t.Errorf("expected category binding_mcp_missing, got %s", warnings[0].Category)
	}
	if warnings[0].MCPID != "nonexistent_mcp" {
		t.Errorf("expected MCPID nonexistent_mcp, got %s", warnings[0].MCPID)
	}
}

func TestValidateRoleToolBindings_BindingToolMissing(t *testing.T) {
	roles := map[string]*Role{
		"r1": {
			ID: "r1",
			BoundMCPBindings: []MCPBinding{
				{MCPID: "mcp1", AllowedTools: []string{"real_tool", "ghost_tool"}},
			},
		},
	}
	mcps := map[string]*MCPDef{
		"mcp1": {ID: "mcp1", AvailableTools: []string{"real_tool", "other_tool"}},
	}
	warnings := ValidateRoleToolBindings(roles, mcps)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if warnings[0].Category != "binding_tool_missing" {
		t.Errorf("expected category binding_tool_missing, got %s", warnings[0].Category)
	}
	if warnings[0].Tool != "ghost_tool" {
		t.Errorf("expected Tool ghost_tool, got %s", warnings[0].Tool)
	}
}

func TestValidateRoleToolBindings_InstructionGhost(t *testing.T) {
	roles := map[string]*Role{
		"airline_agent": {
			ID: "airline_agent",
			BoundMCPBindings: []MCPBinding{
				{MCPID: "tau2-airline", AllowedTools: []string{}},
			},
			Instruction: "The `tau2-airline` MCP tools (e.g. get_reservation_details, " +
				"update_reservation_flights, search_direct_flight, get_user_details, " +
				"find_user_id_by_name_zip) are the ONLY way to read or modify airline data.",
		},
	}
	mcps := map[string]*MCPDef{
		"tau2-airline": {
			ID:             "tau2-airline",
			AvailableTools: []string{"get_reservation_details", "update_reservation_flights", "search_direct_flight", "get_user_details", "book_reservation"},
		},
	}
	warnings := ValidateRoleToolBindings(roles, mcps)

	// find_user_id_by_name_zip is a ghost — it's in the instruction but not in AvailableTools.
	var ghostWarnings []ToolBindingWarning
	for _, w := range warnings {
		if w.Category == "instruction_ghost" {
			ghostWarnings = append(ghostWarnings, w)
		}
	}
	if len(ghostWarnings) != 1 {
		t.Fatalf("expected 1 instruction_ghost warning, got %d: %v", len(ghostWarnings), ghostWarnings)
	}
	if ghostWarnings[0].Tool != "find_user_id_by_name_zip" {
		t.Errorf("expected ghost tool find_user_id_by_name_zip, got %s", ghostWarnings[0].Tool)
	}
	if ghostWarnings[0].RoleID != "airline_agent" {
		t.Errorf("expected RoleID airline_agent, got %s", ghostWarnings[0].RoleID)
	}
}

func TestValidateRoleToolBindings_SkipWhenNoToolsDeclared(t *testing.T) {
	// If no MCP declares AvailableTools (all discovered at runtime),
	// the textual check should be skipped entirely.
	roles := map[string]*Role{
		"r1": {
			ID:          "r1",
			Instruction: "Use `some_ghost_tool_xyz` to do things.",
			BoundMCPBindings: []MCPBinding{
				{MCPID: "mcp1", AllowedTools: []string{}},
			},
		},
	}
	mcps := map[string]*MCPDef{
		"mcp1": {ID: "mcp1", AvailableTools: nil}, // no tools declared
	}
	warnings := ValidateRoleToolBindings(roles, mcps)
	if len(warnings) != 0 {
		t.Fatalf("expected 0 warnings when no tools declared, got %d: %v", len(warnings), warnings)
	}
}

func TestValidateRoleToolBindings_RulesAlsoScanned(t *testing.T) {
	roles := map[string]*Role{
		"r1": {
			ID:   "r1",
			Rules: "Always call `validate_input_data` before processing.",
			BoundMCPBindings: []MCPBinding{
				{MCPID: "mcp1", AllowedTools: []string{}},
			},
		},
	}
	mcps := map[string]*MCPDef{
		"mcp1": {ID: "mcp1", AvailableTools: []string{"process_data"}},
	}
	warnings := ValidateRoleToolBindings(roles, mcps)
	var ghostWarnings []ToolBindingWarning
	for _, w := range warnings {
		if w.Category == "instruction_ghost" {
			ghostWarnings = append(ghostWarnings, w)
		}
	}
	if len(ghostWarnings) != 1 {
		t.Fatalf("expected 1 ghost warning from Rules, got %d: %v", len(ghostWarnings), ghostWarnings)
	}
	if ghostWarnings[0].Tool != "validate_input_data" {
		t.Errorf("expected validate_input_data, got %s", ghostWarnings[0].Tool)
	}
}

func TestValidateRoleToolBindings_NonToolBackticksIgnored(t *testing.T) {
	// Backtick-quoted tokens that don't look like tool names should not warn.
	roles := map[string]*Role{
		"r1": {
			ID:          "r1",
			Instruction: "Set `exit_code` to 0 and `file_path` to /tmp/out.",
			BoundMCPBindings: []MCPBinding{
				{MCPID: "mcp1", AllowedTools: []string{}},
			},
		},
	}
	mcps := map[string]*MCPDef{
		"mcp1": {ID: "mcp1", AvailableTools: []string{"real_tool"}},
	}
	warnings := ValidateRoleToolBindings(roles, mcps)
	for _, w := range warnings {
		if w.Category == "instruction_ghost" {
			t.Errorf("unexpected ghost warning for non-tool token: %s", w.Tool)
		}
	}
}

func TestValidateRoleToolBindings_EmptyInputs(t *testing.T) {
	if w := ValidateRoleToolBindings(nil, nil); len(w) != 0 {
		t.Errorf("expected 0 warnings for nil inputs, got %d", len(w))
	}
	if w := ValidateRoleToolBindings(map[string]*Role{}, map[string]*MCPDef{}); len(w) != 0 {
		t.Errorf("expected 0 warnings for empty inputs, got %d", len(w))
	}
}

func TestToolBindingWarning_String(t *testing.T) {
	tests := []struct {
		w    ToolBindingWarning
		want string
	}{
		{
			ToolBindingWarning{RoleID: "r1", MCPID: "m1", Category: "binding_mcp_missing"},
			`role "r1": binding references MCP "m1" which is not registered`,
		},
		{
			ToolBindingWarning{RoleID: "r1", MCPID: "m1", Tool: "ghost", Category: "binding_tool_missing"},
			`role "r1": binding MCP "m1" AllowedTools lists "ghost" but MCP.AvailableTools does not contain it`,
		},
		{
			ToolBindingWarning{RoleID: "r1", Tool: "ghost", Category: "instruction_ghost"},
			`role "r1": instruction/rules reference tool "ghost" but no registered MCP provides it — likely a ghost tool name`,
		},
	}
	for _, tt := range tests {
		got := tt.w.String()
		if got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}
