package core

import (
	"testing"
)

// C5 regression: Auditor step must not inherit parent's write tools, and the
// inherited MCPs/allowlists must be deep copies (not shared references).
//
// Bug: The auditor step inherited orig.AdditionalMCPs and
// orig.AdditionalToolAllowlists directly — both a slice and a map, which are
// reference types in Go. This meant:
// 1. The auditor got write tools (e.g., transfer_payment, delete_user) from
//    the parent step, allowing a confused/compromised auditor to mutate state.
// 2. Mutations to the auditor's tools would affect the parent step (and vice
//    versa) via the shared backing array / map reference.

func TestAuditorAllowedTools_WriteToolsFiltered(t *testing.T) {
	parentMCPs := []string{"banking", "search"}
	parentAllowlists := map[string][]string{
		"banking": {"get_balance", "transfer_payment", "delete_account", "get_statement"},
		"search":  {"query", "create_index"},
	}

	auditMCPs, auditAllowlists := AuditorAllowedTools(parentMCPs, parentAllowlists)
	_ = auditMCPs // MCPs are not filtered (auditor needs same data sources)

	// Write tools should be filtered out
	for _, tool := range auditAllowlists["banking"] {
		if IsWriteMCPTool(tool) {
			t.Errorf("write tool %q should be filtered from auditor allowlist", tool)
		}
	}
	for _, tool := range auditAllowlists["search"] {
		if IsWriteMCPTool(tool) {
			t.Errorf("write tool %q should be filtered from auditor allowlist", tool)
		}
	}

	// Read tools should be preserved
	foundBalance := false
	for _, tool := range auditAllowlists["banking"] {
		if tool == "get_balance" {
			foundBalance = true
		}
	}
	if !foundBalance {
		t.Errorf("read tool 'get_balance' should be preserved in auditor allowlist")
	}

	foundStatement := false
	for _, tool := range auditAllowlists["banking"] {
		if tool == "get_statement" {
			foundStatement = true
		}
	}
	if !foundStatement {
		t.Errorf("read tool 'get_statement' should be preserved in auditor allowlist")
	}

	foundQuery := false
	for _, tool := range auditAllowlists["search"] {
		if tool == "query" {
			foundQuery = true
		}
	}
	if !foundQuery {
		t.Errorf("read tool 'query' should be preserved in auditor allowlist")
	}
}

func TestAuditorAllowedTools_SliceCopyNotShared(t *testing.T) {
	// Mutating the returned MCPs slice must not affect the parent.
	parentMCPs := []string{"mcp1", "mcp2"}
	auditMCPs, _ := AuditorAllowedTools(parentMCPs, nil)

	// Mutate the audit copy
	if len(auditMCPs) > 0 {
		auditMCPs[0] = "MUTATED"
	}

	// Parent should be unaffected
	if parentMCPs[0] == "MUTATED" {
		t.Errorf("mutating audit MCPs affected parent — slice was not copied")
	}
}

func TestAuditorAllowedTools_MapCopyNotShared(t *testing.T) {
	// Mutating the returned allowlists map must not affect the parent.
	parentAllowlists := map[string][]string{
		"mcp1": {"read_tool", "write_tool"},
	}
	_, auditAllowlists := AuditorAllowedTools(nil, parentAllowlists)

	// Mutate the audit copy
	auditAllowlists["mcp1"] = append(auditAllowlists["mcp1"], "INJECTED")
	auditAllowlists["new_key"] = []string{"new"}

	// Parent should be unaffected
	if len(parentAllowlists["mcp1"]) != 2 {
		t.Errorf("mutating audit allowlist affected parent — map was not copied: parent has %d tools", len(parentAllowlists["mcp1"]))
	}
	if _, exists := parentAllowlists["new_key"]; exists {
		t.Errorf("adding key to audit allowlist affected parent — map was not copied")
	}
}

func TestAuditorAllowedTools_EmptyInput(t *testing.T) {
	auditMCPs, auditAllowlists := AuditorAllowedTools(nil, nil)

	if len(auditMCPs) != 0 {
		t.Errorf("nil MCPs should return empty slice, got %d", len(auditMCPs))
	}
	if len(auditAllowlists) != 0 {
		t.Errorf("nil allowlists should return empty map, got %d", len(auditAllowlists))
	}
}

func TestAuditorAllowedTools_AllWriteToolsFilteredToEmpty(t *testing.T) {
	// If all tools are write tools, the allowlist should be empty (not nil).
	parentAllowlists := map[string][]string{
		"banking": {"transfer_payment", "delete_account", "close_account"},
	}
	_, auditAllowlists := AuditorAllowedTools(nil, parentAllowlists)

	if auditAllowlists["banking"] == nil {
		t.Errorf("filtered allowlist should be empty slice, not nil")
	}
	if len(auditAllowlists["banking"]) != 0 {
		t.Errorf("all-write-tool allowlist should be empty after filtering, got %d tools", len(auditAllowlists["banking"]))
	}
}

func TestAuditorAllowedTools_CoreToolsPreserved(t *testing.T) {
	// Core tools (write_file, read_file, execute_code) are NOT write MCP tools
	// (they're filesystem ops tracked by StagedWorkspace). They should be
	// preserved in the auditor's allowlist.
	parentAllowlists := map[string][]string{
		"core": {"read_file", "write_file", "execute_code"},
	}
	_, auditAllowlists := AuditorAllowedTools(nil, parentAllowlists)

	if len(auditAllowlists["core"]) != 3 {
		t.Errorf("core tools should all be preserved, got %d: %v", len(auditAllowlists["core"]), auditAllowlists["core"])
	}
}
