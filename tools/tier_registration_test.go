package tools

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/daybeam/vortex/config"
)

// This file is a permanent regression guard for tools.go's documented history
// of admin-only tools accidentally landing in the public tier: F26
// (2026-07-06, orchestrator_invoke/orchestrator_reindex_memory), the 08-13/15
// addendum's orchestrator_debug_dump incident, and the 2026-08-26 review that
// found orchestrator_get_code_details (unrestricted arbitrary-file-read)
// registered in RegisterTier1. Every admin-sensitive tool this project has
// ever had to move out of the public tier gets a permanent assertion here so
// the next regression is caught by `go test`, not by a manual code review.

func newTestApp(t *testing.T) *App {
	t.Helper()
	reg, err := config.NewRegistry(filepath.Join(t.TempDir(), "config_test.json"))
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return &App{Registry: reg}
}

// adminOnlyTools is the set every one of this project's confirmed public-tier
// leak incidents involved. These must NEVER appear in Tier1 (public), even if
// they have been moved to subsystem actions — the list serves as a permanent
// security regression guard. Add to this list any time a future incident is
// found and fixed -- do not just fix the code, extend this list too.
var adminOnlyTools = []string{
	"orchestrator_invoke", // F26, 2026-07-06
	// orchestrator_reindex_memory was migrated to admin.reindex_memory subsystem (2026-09-04)
	"orchestrator_debug_dump",       // 08-13/15 addendum (moved to admin.debug_dump subsystem 2026-09-20)
	"orchestrator_get_code_details", // 2026-08-26 review (moved to intel.get_code_details subsystem 2026-09-20)
	"orchestrator_run_command",
}

// adminTier2Tools is the subset of admin-sensitive tools that are still
// registered as top-level MCP tools in Tier2. Tools moved to subsystem
// actions are excluded — they are invoked via orchestrator_invoke instead.
// The Promoter may hot-promote subsystem actions back to top-level at
// runtime, but the static registration set is what this test verifies.
var adminTier2Tools = []string{
	"orchestrator_invoke",
	"orchestrator_run_command",
}

func TestRegisterTier1_NeverExposesAdminOnlyTools(t *testing.T) {
	s := server.NewMCPServer("test-public", "0.0.0")
	app := newTestApp(t)
	RegisterTier1(s, app)

	toolList := s.ListTools()
	for _, name := range adminOnlyTools {
		if _, ok := toolList[name]; ok {
			t.Errorf("SECURITY REGRESSION: admin-only tool %q is registered on the public-tier server (RegisterTier1) -- see this file's package doc comment for the history of why this must never happen", name)
		}
	}
}

func TestRegisterTier2_ExposesAllAdminOnlyTools(t *testing.T) {
	s := server.NewMCPServer("test-admin", "0.0.0")
	app := newTestApp(t)
	RegisterTier2(s, app)

	toolList := s.ListTools()
	for _, name := range adminTier2Tools {
		if _, ok := toolList[name]; !ok {
			t.Errorf("expected admin-only tool %q to be registered on the admin-tier server (RegisterTier2), but it's missing -- if this was intentionally removed, also remove it from adminTier2Tools in this test file", name)
		}
	}
}

// TestRegisterTier1_SubmitDecisionRestrictsChoices is a narrower regression
// test for F26's second half: orchestrator_submit_decision IS present on the
// public tier (by design, so public callers can unblock a stuck task), but
// its handler must reject choices beyond skip/abort/retry. This doesn't call
// the handler directly (that needs a running Scheduler) -- it only confirms
// the tool itself is present on Tier1, which is the precondition for the
// handler-level restriction in tools/subsystems.go's app.Tier check to even
// matter. See subsystems_update_system_test.go-style tests for handler-level
// coverage if this needs to go deeper.
func TestRegisterTier1_HasSubmitDecision(t *testing.T) {
	s := server.NewMCPServer("test-public", "0.0.0")
	app := newTestApp(t)
	RegisterTier1(s, app)

	if _, ok := s.ListTools()["orchestrator_submit_decision"]; !ok {
		t.Errorf("expected orchestrator_submit_decision to be present on the public tier (F26 design) -- if this changed intentionally, update this test")
	}
}

// TestServerInstructionsAdmin_OnlyNamesRegisteredTools applies the same
// "never advertise a tool the caller cannot see" invariant to the admin-tier
// instructions. Only the advertised-subset direction is checked here: the admin
// string documents how to drive the engine, not the whole surface, so naming
// fewer than all 12 tools is legitimate.
func TestServerInstructionsAdmin_OnlyNamesRegisteredTools(t *testing.T) {
	s := server.NewMCPServer("test-admin", "0.0.0")
	app := newTestApp(t)
	app.InitArchive(t.TempDir())
	t.Cleanup(app.StopArchive)
	RegisterAll(s, app)
	registered := s.ListTools()

	for _, name := range tierToolNameRe.FindAllString(ServerInstructions, -1) {
		if name == "orchestrator_mcp" { // serverInfo name, not a tool
			continue
		}
		if _, ok := registered[name]; !ok {
			t.Errorf("ServerInstructions advertises %q but RegisterAll does not register it", name)
		}
	}
}

// tierToolNameRe matches a tool identifier as the instruction strings spell them.
var tierToolNameRe = regexp.MustCompile(`orchestrator_[a-z0-9_]+`)

// TestServerInstructionsPublic_MatchesTier1ToolSet checks the invariant stated
// in server_instructions.go's own comment -- "referencing a tool the caller
// cannot see is confusing at best and at worst invites a failed call to a
// nonexistent tool" -- in both directions.
// Regression (found 2026-11-03 while reconciling the documented tool surface
// against tools/list): the public instructions advertised
// orchestrator_analyze_task, a name that has never been registered anywhere in
// this module, and omitted orchestrator_context_search, which RegisterTier1
// does register. A public client following the instructions would call a tool
// that does not exist and never discover one that does.
func TestServerInstructionsPublic_MatchesTier1ToolSet(t *testing.T) {
	s := server.NewMCPServer("test-public", "0.0.0")
	app := newTestApp(t)
	// orchestrator_context_search only registers when app.Archive is wired
	// (tools_task.go:634), which every main_*.go does via InitArchive. Without
	// it the fixture would serve a smaller tool set than production.
	app.InitArchive(t.TempDir())
	t.Cleanup(app.StopArchive)
	RegisterTier1(s, app)
	registered := s.ListTools()

	mentioned := map[string]bool{}
	for _, name := range tierToolNameRe.FindAllString(ServerInstructionsPublic, -1) {
		if name == "orchestrator_mcp" { // serverInfo name, not a tool
			continue
		}
		mentioned[name] = true
	}

	for name := range mentioned {
		if _, ok := registered[name]; !ok {
			t.Errorf("ServerInstructionsPublic advertises %q but RegisterTier1 does not register it -- public clients will call a tool that does not exist", name)
		}
	}
	for name := range registered {
		if !mentioned[name] {
			t.Errorf("RegisterTier1 registers %q but ServerInstructionsPublic never mentions it -- callers have no reason to reach for it", name)
		}
	}
}
