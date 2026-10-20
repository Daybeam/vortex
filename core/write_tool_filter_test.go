package core

import (
	"testing"
)

// Regression: Auditor step must not inherit parent's write tools, and the
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
	// Fix: MCPs now need an allowlist with surviving read tools to be included.
	parentMCPs := []string{"mcp1", "mcp2"}
	parentAllowlists := map[string][]string{
		"mcp1": {"read_tool"},
		"mcp2": {"query"},
	}
	auditMCPs, _ := AuditorAllowedTools(parentMCPs, parentAllowlists)

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
	// Fix: if all tools are write tools, the MCP should be
	// SKIPPED entirely (not in mcps, not in allowlists). Previously, the empty
	// allowlist was kept, and validator.go treated empty as "no restriction"
	// → full access → auditor could call write tools.
	parentMCPs := []string{"banking"}
	parentAllowlists := map[string][]string{
		"banking": {"transfer_payment", "delete_account", "close_account"},
	}
	auditMCPs, auditAllowlists := AuditorAllowedTools(parentMCPs, parentAllowlists)

	// MCP should be skipped — not in the returned mcps list
	for _, mcp := range auditMCPs {
		if mcp == "banking" {
			t.Errorf("MCP 'banking' with all-write tools should be skipped, not included in mcps")
		}
	}
	// MCP should not be in allowlists either
	if _, exists := auditAllowlists["banking"]; exists {
		t.Errorf("MCP 'banking' with all-write tools should be skipped, not in allowlists")
	}
}

func TestAuditorAllowedTools_NoAllowlistMeansSkipped(t *testing.T) {
	// Fix: MCPs without an explicit allowlist entry are
	// skipped (whitelist approach). Previously they were inherited entirely
	// with all write tools.
	parentMCPs := []string{"unknown_mcp"}
	// No allowlist entry for "unknown_mcp"
	auditMCPs, auditAllowlists := AuditorAllowedTools(parentMCPs, nil)

	if len(auditMCPs) != 0 {
		t.Errorf("MCP without allowlist should be skipped (whitelist), got %d mcps: %v", len(auditMCPs), auditMCPs)
	}
	if len(auditAllowlists) != 0 {
		t.Errorf("no allowlists should be returned for MCPs without entries, got %d", len(auditAllowlists))
	}
}

func TestIsWriteMCPTool_AppPrefixedNames(t *testing.T) {
	// Fix: handle app-prefixed tool names (e.g., "app__send_message").
	// The write verb appears after "app__", so we strip it before checking.
	writeAppTools := []string{"app__send_message", "app__create_order", "app__delete_user", "app__transfer_funds"}
	for _, tool := range writeAppTools {
		if !IsWriteMCPTool(tool) {
			t.Errorf("app-prefixed write tool %q should be detected as write", tool)
		}
	}
	readAppTools := []string{"app__get_status", "app__query_list", "app__search_items"}
	for _, tool := range readAppTools {
		if IsWriteMCPTool(tool) {
			t.Errorf("app-prefixed read tool %q should NOT be detected as write", tool)
		}
	}
}

func TestIsWriteMCPTool_AddedPrefixes(t *testing.T) {
	// Fix: verify newly added write prefixes are detected.
	addedWriteTools := []string{"add_item", "pay_invoice", "like_post", "follow_user",
		"mark_read", "upload_file", "execute_query", "run_job", "publish_article"}
	for _, tool := range addedWriteTools {
		if !IsWriteMCPTool(tool) {
			t.Errorf("tool %q with newly added write prefix should be detected as write", tool)
		}
	}
}

func TestAuditorAllowedTools_CoreToolsPreserved(t *testing.T) {
	// Core tools (write_file, read_file, execute_code) are NOT write MCP tools
	// (they're filesystem ops tracked by StagedWorkspace). They should be
	// preserved in the auditor's allowlist.
	parentMCPs := []string{"core"}
	parentAllowlists := map[string][]string{
		"core": {"read_file", "write_file", "execute_code"},
	}
	_, auditAllowlists := AuditorAllowedTools(parentMCPs, parentAllowlists)

	if len(auditAllowlists["core"]) != 3 {
		t.Errorf("core tools should all be preserved, got %d: %v", len(auditAllowlists["core"]), auditAllowlists["core"])
	}
}

// TestIsWriteMCPTool_GeneralizedNamespaceStripping verifies that IsWriteMCPTool
// handles arbitrary namespace prefixes and separators (__, ., /), not just the
// hardcoded "app__" prefix.
//
// Before the fix: only "app__" was stripped. Tools like "mcp1.send_message"
// or "service/delete_user" were not recognized as write tools because the
// namespace prefix wasn't stripped.
func TestIsWriteMCPTool_GeneralizedNamespaceStripping(t *testing.T) {
	// Write tools with various namespace separators and prefixes.
	writeTools := []string{
		// __ separator with non-app prefix
		"mcp__send_message",
		"tool__create_order",
		"service__delete_user",
		// . separator
		"mcp1.send_message",
		"tool.create_order",
		"service.delete_user",
		// / separator
		"mcp1/send_message",
		"tool/create_order",
		"service/delete_user",
		// multiple separators (last one wins)
		"a__b__send_message",
		"mcp.tool.update_record",
		"svc/api/post_data",
		// original app__ still works
		"app__send_message",
	}
	for _, tool := range writeTools {
		if !IsWriteMCPTool(tool) {
			t.Errorf("namespaced write tool %q should be detected as write", tool)
		}
	}

	// Read tools with various namespace separators — must NOT be detected as write.
	readTools := []string{
		"mcp__get_status",
		"mcp1.query_list",
		"service/search_items",
		"app__get_status",
		"a__b__search_items",
	}
	for _, tool := range readTools {
		if IsWriteMCPTool(tool) {
			t.Errorf("namespaced read tool %q should NOT be detected as write", tool)
		}
	}
}
