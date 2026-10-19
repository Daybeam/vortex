// Regression + containment tests for tier 1(b) pre-submission planning
// (S6, 2026-10-18).
//
// Bug being pinned: `config.system.enable_planner_first` existed and was read,
// but `SubmitWithSessionIR` had no branch that consumed it. Every submission
// whose main agent emitted a single dependency-free step fell straight into
// the smart-routing fast-path, so the task_planner role could never run and
// task-level DAG construction was unreachable. The field was dead config.
//
// These tests pin the CONTAINMENT contract, which is what matters most here:
// SubmitWithSessionIR is the entry point of every submission flow
// (impact=HIGH), so planning must never turn a working submission into a
// failing one.
//
// See eval-kit docs/52-planner-wiring-design.md §S6.

package core

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// TestRequestPlanning_FlagOffSkips verifies the default-off guarantee: with
// enable_planner_first unset, no planner role lookup happens at all.
func TestRequestPlanning_FlagOffSkips(t *testing.T) {
	s, _ := newPlannerTestEngine(t)

	steps, reason := s.RequestPlanning("task-x", "appworld_agent", "do the thing", 0)
	if steps != nil {
		t.Fatalf("flag off must not produce a plan, got %d steps", len(steps))
	}
	if reason == "" {
		t.Fatalf("flag off must report a reason")
	}
	if reason != "enable_planner_first is off" {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

// TestRequestPlanning_RoleMissingSkips covers the exact state of every
// AppWorld config on disk: flag on but no task_planner role. The caller must
// get a reason, not an error and not a panic.
func TestRequestPlanning_RoleMissingSkips(t *testing.T) {
	s, reg := newPlannerTestEngine(t)
	reg.System.EnablePlannerFirst = true

	steps, reason := s.RequestPlanning("task-x", "appworld_agent", "do the thing", 0)
	if steps != nil {
		t.Fatalf("missing role must not produce a plan, got %d steps", len(steps))
	}
	if reason != "task_planner role not present in config" {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

// TestRequestPlanning_EmptyTaskSkips makes sure a config with the role present
// still refuses to spend an LLM call on an empty request.
func TestRequestPlanning_EmptyTaskSkips(t *testing.T) {
	s, reg := newPlannerTestEngine(t)
	reg.System.EnablePlannerFirst = true
	reg.Roles["task_planner"] = &config.Role{
		ID:          "task_planner",
		Instruction: "plan the DAG",
	}

	steps, reason := s.RequestPlanning("task-x", "appworld_agent", "   ", 0)
	if steps != nil {
		t.Fatalf("empty task must not produce a plan")
	}
	if reason != "empty task" {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

// TestRequestPlanning_NoSpawnerDegrades verifies the engine-initialisation
// guard. A half-constructed engine must report unavailability rather than
// nil-pointer panic on the submission hot path.
func TestRequestPlanning_NoSpawnerDegrades(t *testing.T) {
	reg := &config.Registry{
		Roles:  map[string]*config.Role{"task_planner": {ID: "task_planner", Instruction: "x"}},
		System: config.SystemSettings{EnablePlannerFirst: true},
	}
	s, _ := newPlannerTestEngine(t)
	s.registry = reg
	s.spawner = nil

	steps, reason := s.RequestPlanning("task-x", "appworld_agent", "do the thing", 0)
	if steps != nil {
		t.Fatalf("nil spawner must not produce a plan")
	}
	if reason != "engine not initialised" {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

// TestPlannerResultText_StepsObject covers the 2026-10-18 AppWorld failure: a
// planner that answers with the bare contract object {"steps": [...]} lands in
// Result["steps"] (schemas.ParseOutput fills the map field, not "text"), and
// the old code rendered it with fmt.Sprintf("%v") as "map[steps:[map[...]]]",
// which is not JSON. A perfectly valid plan was rejected with "could not
// recover valid JSON from planner output".
func TestPlannerResultText_StepsObject(t *testing.T) {
	res := &SpawnResult{Output: schemas.SubagentOutput{Result: map[string]any{
		"steps": []any{
			map[string]any{
				"id":            "s1",
				"role_id":       "appworld_agent",
				"task":          "do the thing",
				"allowed_tools": []any{"venmo__login"},
			},
		},
	}}}

	raw := plannerResultText(res)
	if strings.Contains(raw, "map[") {
		t.Fatalf("plannerResultText leaked Go-syntax map printing: %q", raw)
	}
	// Must be parseable as JSON directly — a string that merely *contains*
	// a JSON fragment after Go-map noise would still fail ParsePlanDAG, so
	// checking json.Valid here is what makes this test bite. Verified by
	// reverting the "steps" branch: with only the final whole-map marshal
	// the output becomes {"steps":...} anyway, so that revert does NOT prove
	// much — the case that actually broke in production was Result holding a
	// nested map ({"steps": map[...]}), covered below.
	if !json.Valid([]byte(raw)) {
		t.Fatalf("plannerResultText must return valid JSON, got %q", raw)
	}
	if _, err := schemas.ParsePlanDAG(raw, schemas.PlanParseOptions{
		AllowedRoles: map[string]bool{"appworld_agent": true},
		ToolCatalog:  map[string]map[string]bool{"appworld": {"venmo__login": true}},
		SingleMCP:    "appworld",
	}); err != nil {
		t.Fatalf("valid plan must round-trip; got %v (raw=%q)", err, raw)
	}
}

// TestPlannerResultText_NestedMapIsNotGoPrinted pins the exact production
// shape of the 2026-10-18 failure. schemas.ParseOutput had already
// deep-decoded the reply into nested map[string]any values, so the only
// faithful recovery is encoding/json marshalling — fmt.Sprintf("%v") yields
// "map[steps:[map[allowed_tools:[...]]]]" and ParsePlanDAG rejects it.
func TestPlannerResultText_NestedMapIsNotGoPrinted(t *testing.T) {
	res := &SpawnResult{Output: schemas.SubagentOutput{Result: map[string]any{
		"steps": []map[string]any{{
			"id":            "s1",
			"role_id":       "appworld_agent",
			"task":          "do the thing",
			"allowed_tools": []string{"venmo__login"},
		}},
	}}}

	raw := plannerResultText(res)
	if strings.Contains(raw, "map[") {
		t.Fatalf("Go-syntax map leaked into planner JSON: %q", raw)
	}
	if !json.Valid([]byte(raw)) {
		t.Fatalf("not valid JSON: %q", raw)
	}
}

// TestPlannerAllowedRoles_ExcludesLeakyDefaults pins the allowlist the planner
// is shown. Two exclusions matter:
//   - task_planner / decision_arbiter / sop_generator: planning loops
//   - orchestrator_default: injected into EVERY registry by config.loader, so
//     its presence says nothing about the deployment; it has no MCP bindings
//     and allow_dynamic_mcps=true, so a step assigned to it cannot do
//     AppWorld work. A planner that names it has misread the catalog, and
//     silently accepting it turns "planner was confused" into a step that
//     burns a full turn and fails.
func TestPlannerAllowedRoles_ExcludesLeakyDefaults(t *testing.T) {
	s, reg := newPlannerTestEngine(t)

	reg.Roles["appworld_agent"] = &config.Role{ID: "appworld_agent", BaseCapability: "appworld"}
	reg.Roles["auditor"] = &config.Role{ID: "auditor", BaseCapability: "audit"}
	reg.Roles["task_planner"] = &config.Role{ID: "task_planner", BaseCapability: "plan"}
	reg.Roles["decision_arbiter"] = &config.Role{ID: "decision_arbiter", BaseCapability: "arbitrate"}
	reg.Roles["sop_generator"] = &config.Role{ID: "sop_generator", BaseCapability: "sop"}
	reg.Roles["orchestrator_default"] = &config.Role{ID: "orchestrator_default", BaseCapability: "general"}

	_, allowed := s.plannerAllowedRoles()

	if !allowed["appworld_agent"] {
		t.Errorf("appworld_agent must be assignable")
	}
	if !allowed["auditor"] {
		t.Errorf("auditor must stay assignable (other deployments use it)")
	}
	for _, id := range []string{"task_planner", "decision_arbiter", "sop_generator", "orchestrator_default"} {
		if allowed[id] {
			t.Errorf("%s must NOT be assignable by the planner", id)
		}
	}
}

// TestPlannerAllowedRoles_DefaultRoleRejectedEndToEnd verifies the exclusion
// actually rejects a plan naming orchestrator_default, rather than merely
// hiding it from the prompt.
func TestPlannerAllowedRoles_DefaultRoleRejectedEndToEnd(t *testing.T) {
	s, reg := newPlannerTestEngine(t)
	reg.Roles["appworld_agent"] = &config.Role{ID: "appworld_agent"}
	reg.Roles["orchestrator_default"] = &config.Role{ID: "orchestrator_default"}

	raw := `{"steps":[{"id":"s1","role_id":"orchestrator_default","task":"2+2","allowed_tools":["execute_code"]}]}`
	if _, err := s.parsePlannerOutput(raw, "appworld_agent"); err == nil {
		t.Fatal("a plan assigning orchestrator_default must be rejected")
	}
}

// TestPlannerFirstEnabled_ReadsConfig confirms the flag is actually wired to
// the registry field rather than hardcoded.
func TestPlannerFirstEnabled_ReadsConfig(t *testing.T) {
	s, reg := newPlannerTestEngine(t)

	if s.plannerFirstEnabled() {
		t.Fatalf("default must be false")
	}
	reg.System.EnablePlannerFirst = true
	if !s.plannerFirstEnabled() {
		t.Fatalf("flag must follow the registry")
	}
}

// TestPlanTerminalStepIDs pins the terminal-selection used when reattaching a
// caller's Deliver contract to a planner-produced DAG.
func TestPlanTerminalStepIDs(t *testing.T) {
	steps := []schemas.StepInput{
		{ID: "a"},
		{ID: "b"},
		{ID: "c", DependsOn: []string{"a", "b"}},
		{ID: "d", DependsOn: []string{"c"}},
	}
	got := planTerminalStepIDs(steps)

	if !got["d"] {
		t.Errorf("d should be terminal")
	}
	for _, id := range []string{"a", "b", "c"} {
		if got[id] {
			t.Errorf("%s should NOT be terminal", id)
		}
	}
}

// TestPlanTerminalStepIDs_ParallelLeaves verifies fan-out leaves are all
// considered terminal — a plan with two independent finishing steps must let
// both inherit the Deliver contract.
func TestPlanTerminalStepIDs_ParallelLeaves(t *testing.T) {
	steps := []schemas.StepInput{
		{ID: "root"},
		{ID: "left", DependsOn: []string{"root"}},
		{ID: "right", DependsOn: []string{"root"}},
	}
	got := planTerminalStepIDs(steps)
	if !got["left"] || !got["right"] {
		t.Fatalf("both leaves must be terminal, got %v", got)
	}
	if got["root"] {
		t.Fatalf("root must not be terminal")
	}
}

// TestPlannerSingleMCP_OnlyWhenUnambiguous guards the mcp_id inference: a role
// binding two MCPs must NOT get a default, otherwise the planner's steps could
// be attributed to the wrong server.
func TestPlannerSingleMCP_OnlyWhenUnambiguous(t *testing.T) {
	s, reg := newPlannerTestEngine(t)

	reg.Roles["one_mcp"] = &config.Role{
		ID: "one_mcp",
		BoundMCPBindings: []config.MCPBinding{
			{MCPID: "appworld"},
		},
	}
	if got := s.plannerSingleMCP("one_mcp"); got != "appworld" {
		t.Fatalf("single binding should infer appworld, got %q", got)
	}

	reg.Roles["two_mcp"] = &config.Role{
		ID: "two_mcp",
		BoundMCPBindings: []config.MCPBinding{
			{MCPID: "appworld"}, {MCPID: "phone"},
		},
	}
	if got := s.plannerSingleMCP("two_mcp"); got != "" {
		t.Fatalf("ambiguous bindings must not infer an MCP, got %q", got)
	}
}

// TestPlannerToolCatalog_EmptyOnColdEngine documents the cold-engine behavior
// after the T7 fix: on a fresh engine no MCP handshake has run yet, so
// AvailableTools is empty and the planner gets no catalog. Previously this
// rejected plans that named tools ("tool catalog unavailable"). Now the plan
// is accepted with unverified tools — they will be intersected with discovered
// tools at execution time (validator.go ResolveFinalMCPs).
func TestPlannerToolCatalog_EmptyOnColdEngine(t *testing.T) {
	s, reg := newPlannerTestEngine(t)

	reg.Roles["appworld_agent"] = &config.Role{
		ID:               "appworld_agent",
		BoundMCPBindings: []config.MCPBinding{{MCPID: "appworld"}},
	}
	// Registered with a full tool set but NOT yet discovered (AvailableTools
	// is only filled by spawner.buildMCPServers during a real step).
	reg.MCPs["appworld"] = &config.MCPDef{
		ID:             "appworld",
		AvailableTools: nil,
	}

	text, catalog := s.plannerToolCatalog("appworld_agent")
	if text != "" {
		t.Errorf("cold engine must render an empty catalog, got:/n%s", text)
	}
	if len(catalog) != 0 {
		t.Errorf("cold engine must yield no validatable catalog, got %v", catalog)
	}

	// T7 fix: a plan naming tools is now ACCEPTED on a cold engine (previously
	// rejected with "tool catalog unavailable"). The tools are marked
	// "unverified" and will be intersected with discovered tools at execution.
	raw := `{"steps":[{"id":"s1","role_id":"appworld_agent","task":"x","allowed_tools":["venmo__login"]}]}`
	inputs, err := schemas.ParsePlanDAG(raw, schemas.PlanParseOptions{
		AllowedRoles: map[string]bool{"appworld_agent": true},
		ToolCatalog:  catalog,
		SingleMCP:    "appworld",
	})
	if err != nil {
		t.Fatalf("T7: cold engine should accept tool-naming plan (unverified), got error: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("expected 1 step, got %d", len(inputs))
	}
	tools := inputs[0].AdditionalToolAllowlists["appworld"]
	if len(tools) != 1 || tools[0] != "venmo__login" {
		t.Errorf("unverified tool should pass through as-is, got %v", tools)
	}
}
