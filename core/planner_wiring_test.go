package core

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// Regression tests for the task-level planner wiring (2026-10-18).
//
// Bug being pinned: `RequestAutonomousAbort` offered a "replan" decision
// option whenever `tryPlannerReplan` returned non-empty, but
// `SubmitDecisionWithPayload` had NO case for "replan" — the switch fell
// through to `default`, hit the prefix matchers, and returned
// `unknown choice "replan"`. Even if it had not errored, the planner's
// markdown output was never parsed into steps, so the DAG could not change.
//
// See eval-kit docs/52-planner-wiring-design.md (checkpoints B2/B3/B4).

func newPlannerTestEngine(t *testing.T) (*DirectedEngine, *config.Registry) {
	t.Helper()
	reg := &config.Registry{
		Roles: make(map[string]*config.Role),
		MCPs:  make(map[string]*config.MCPDef),
		System: config.SystemSettings{
			EnablePlannerFirst: false, // default must stay off
		},
	}
	logDir := t.TempDir()
	outDir := t.TempDir()
	logger := mustNewLogger(t, logDir, &config.SystemSettings{})
	t.Cleanup(func() { logger.Close() })
	s := NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	return s, reg
}

// fixtureGraph builds a one-step graph with a pending "replan" decision whose
// context carries a planner payload — the exact shape RequestAutonomousAbort
// now produces.
func fixtureGraph(t *testing.T, plannerPayload string) *schemas.TaskGraph {
	t.Helper()
	graph := &schemas.TaskGraph{
		TaskID:           "task-replan-1",
		Steps:            make(map[string]*schemas.Step),
		Status:           schemas.GraphRunning,
		PendingDecisions: nil,
		ContextTree:      map[string]*schemas.ContextNode{"root": {ID: "root", StepIDs: []string{"s1"}}},
		CurrentNodeID:    "root",
	}
	graph.Steps["s1"] = &schemas.Step{
		ID:     "s1",
		RoleID: "appworld_agent",
		Task:   "original failing task",
		Status: schemas.StepRunning,
	}
	decCtx := map[string]any{
		"failure_class": string(FailureClassCostOverrun),
		"reason":        "test",
	}
	if plannerPayload != "" {
		decCtx["planner_replan"] = plannerPayload
	}
	graph.PendingDecisions = []*schemas.Decision{{
		ID:      "dec-1",
		StepID:  "s1",
		Type:    schemas.DecisionAutonomousAbortRequested,
		Context: decCtx,
		Options: []string{"replan", "confirm_abort", "continue"},
	}}
	return graph
}

// The core fix: choosing "replan" must actually inject the replacement steps
// and deprecate the failed step.
func TestSubmitDecision_ReplanInjectsSteps(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	steps := []schemas.StepInput{
		{ID: "r1", RoleID: "appworld_agent", Task: "replanned part A"},
		{ID: "r2", RoleID: "appworld_agent", Task: "replanned part B", DependsOn: []string{"r1"}},
	}
	payload, err := json.Marshal(map[string]any{"replacement_steps": steps})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	graph := fixtureGraph(t, string(payload))
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	if err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-1", "replan", ""); err != nil {
		t.Fatalf("replan should succeed, got error: %v", err)
	}

	s.Mu.RLock()
	g := s.graphs[graph.TaskID]
	s.Mu.RUnlock()

	if got := g.Steps["s1"].Status; got != schemas.StepDeprecated {
		t.Fatalf("original step should be deprecated, got %s", got)
	}
	for _, id := range []string{"r1", "r2"} {
		st, ok := g.Steps[id]
		if !ok {
			t.Fatalf("replacement step %q not injected", id)
		}
		if st.Status != schemas.StepPending {
			t.Fatalf("replacement step %q status = %s, want pending", id, st.Status)
		}
	}
	if len(g.PendingDecisions) != 0 {
		t.Fatalf("decision should be consumed, %d remain", len(g.PendingDecisions))
	}
}

// A replan with no payload and no planner output must fail loudly rather
// than silently consuming the decision (the old bug's failure mode).
func TestSubmitDecision_ReplanWithoutPayloadErrors(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	graph := fixtureGraph(t, "") // no planner_replan in context
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-1", "replan", "")
	if err == nil {
		t.Fatal("replan without any replacement steps must error")
	}
	if !strings.Contains(err.Error(), "no payload and no planner output") {
		t.Fatalf("unexpected error: %v", err)
	}

	s.Mu.RLock()
	g := s.graphs[graph.TaskID]
	s.Mu.RUnlock()
	if g.Steps["s1"].Status == schemas.StepDeprecated {
		t.Fatal("graph must be untouched when replan payload is missing")
	}
	if len(g.PendingDecisions) != 1 {
		t.Fatal("decision must remain pending when replan fails")
	}
}

// An explicit `output` surgery payload overrides the stored planner plan.
func TestSubmitDecision_ReplanExplicitPayloadWins(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	stored, _ := json.Marshal(map[string]any{"replacement_steps": []schemas.StepInput{
		{ID: "from_planner", RoleID: "appworld_agent", Task: "planner version"},
	}})
	explicit, _ := json.Marshal(map[string]any{"replacement_steps": []schemas.StepInput{
		{ID: "from_caller", RoleID: "appworld_agent", Task: "caller version"},
	}})

	graph := fixtureGraph(t, string(stored))
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	if err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-1", "replan", string(explicit)); err != nil {
		t.Fatalf("replan with explicit payload failed: %v", err)
	}

	s.Mu.RLock()
	g := s.graphs[graph.TaskID]
	s.Mu.RUnlock()

	if _, ok := g.Steps["from_caller"]; !ok {
		t.Fatal("explicit payload should win and be injected")
	}
	if _, ok := g.Steps["from_planner"]; ok {
		t.Fatal("stored planner plan should be ignored when payload is supplied")
	}
}

// Malformed surgery JSON must not corrupt the graph.
func TestSubmitDecision_ReplanInvalidPayloadErrors(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	graph := fixtureGraph(t, "{not json")
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	if err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-1", "replan", ""); err == nil {
		t.Fatal("invalid replan payload must error")
	}
}

// ─── planner plumbing ─────────────────────────────────────────────────────

// Default must stay off: an unconfigured deployment behaves exactly as before.
func TestPlannerFirstEnabled_DefaultsOff(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()
	if s.plannerFirstEnabled() {
		t.Fatal("EnablePlannerFirst must default to false")
	}
}

// The planner must never be offered task_planner / decision_arbiter /
// sop_generator as assignable roles — that would create planning loops.
func TestPlannerAllowedRoles_ExcludesMetaRoles(t *testing.T) {
	s, reg := newPlannerTestEngine(t)
	defer s.Stop()

	reg.Mu.Lock()
	reg.Roles["appworld_agent"] = &config.Role{ID: "appworld_agent", BaseCapability: "act"}
	reg.Roles["task_planner"] = &config.Role{ID: "task_planner", BaseCapability: "plan"}
	reg.Roles["decision_arbiter"] = &config.Role{ID: "decision_arbiter", BaseCapability: "plan"}
	reg.Roles["sop_generator"] = &config.Role{ID: "sop_generator", BaseCapability: "plan"}
	reg.Mu.Unlock()

	rendered, allowed := s.plannerAllowedRoles()

	if !allowed["appworld_agent"] {
		t.Fatal("appworld_agent should be assignable")
	}
	for _, meta := range []string{"task_planner", "decision_arbiter", "sop_generator"} {
		if allowed[meta] {
			t.Fatalf("meta role %q must not be assignable", meta)
		}
		if strings.Contains(rendered, "`"+meta+"`") {
			t.Fatalf("meta role %q leaked into prompt", meta)
		}
	}
}

// The tool catalog must reflect ONLY discovered tools, so a planner cannot
// name a tool that will not exist at run time.
func TestPlannerToolCatalog_OnlyDiscoveredTools(t *testing.T) {
	s, reg := newPlannerTestEngine(t)
	defer s.Stop()

	reg.Mu.Lock()
	reg.Roles["appworld_agent"] = &config.Role{
		ID: "appworld_agent",
		BoundMCPBindings: []config.MCPBinding{
			{MCPID: "appworld", AllowedTools: []string{}},
		},
	}
	reg.MCPs["appworld"] = &config.MCPDef{
		ID:             "appworld",
		AvailableTools: []string{"venmo__login", "venmo__show_sent_payment_requests"},
		FullToolDefinitions: []schemas.ToolDefinition{
			{Name: "venmo__login", Description: "Log in to Venmo"},
			{Name: "venmo__show_sent_payment_requests", Description: "List sent requests"},
		},
	}
	reg.Mu.Unlock()

	rendered, catalog := s.plannerToolCatalog("appworld_agent", true)

	if len(catalog["appworld"]) != 2 {
		t.Fatalf("catalog should hold 2 tools, got %d", len(catalog["appworld"]))
	}
	if !catalog["appworld"]["venmo__login"] {
		t.Fatal("venmo__login missing from catalog")
	}
	if !strings.Contains(rendered, "venmo__login") {
		t.Fatal("catalog prompt should list tool names")
	}
	if strings.Contains(rendered, "venmo__delete_everything") {
		t.Fatal("catalog invented a tool")
	}
}

// Stage 5 (admin-hiding, 2026-10-24): a non-admin submitter's planner catalog
// must EXCLUDE admin_only MCPs so admin tool names never reach the plan (and
// thus the public-visible task status). An admin submitter still sees them.
func TestPlannerToolCatalog_HidesAdminOnlyForNonAdmin(t *testing.T) {
	s, reg := newPlannerTestEngine(t)
	defer s.Stop()
	reg.Mu.Lock()
	reg.Roles["appworld_agent"] = &config.Role{
		ID: "appworld_agent",
		BoundMCPBindings: []config.MCPBinding{
			{MCPID: "appworld", AllowedTools: []string{}},
		},
	}
	reg.MCPs["appworld"] = &config.MCPDef{
		ID:             "appworld",
		AvailableTools: []string{"venmo__login"},
		// no MountPolicy ⇒ admin_only (the default)
	}
	reg.Mu.Unlock()

	// admin sees the (admin_only) MCP
	_, adminCat := s.plannerToolCatalog("appworld_agent", true)
	if len(adminCat["appworld"]) == 0 {
		t.Fatal("admin planner catalog must include the admin_only MCP")
	}
	// non-admin does NOT — admin tool names stay out of the plan
	_, publicCat := s.plannerToolCatalog("appworld_agent", false)
	if _, ok := publicCat["appworld"]; ok {
		t.Fatal("SECURITY: non-admin planner catalog must exclude admin_only MCPs")
	}
}

// End-to-end of the parse step used by tryPlannerReplan: a realistic planner
// reply (prose + fenced JSON) must become StepInputs.
func TestParsePlannerOutput_RealisticPlannerReply(t *testing.T) {
	s, reg := newPlannerTestEngine(t)
	defer s.Stop()

	reg.Mu.Lock()
	reg.Roles["appworld_agent"] = &config.Role{
		ID:               "appworld_agent",
		BoundMCPBindings: []config.MCPBinding{{MCPID: "appworld"}},
	}
	reg.MCPs["appworld"] = &config.MCPDef{
		ID: "appworld",
		AvailableTools: []string{
			"phone__login", "phone__search_contacts",
			"venmo__login", "venmo__show_sent_payment_requests",
		},
	}
	reg.Mu.Unlock()

	raw := "I will split this into two independent reads.\n\n```json\n" +
		`{"steps":[
		  {"id":"p1","role_id":"appworld_agent","task":"Collect roommate emails",
		   "allowed_tools":["phone__login","phone__search_contacts"]},
		  {"id":"p2","role_id":"appworld_agent","task":"List pending sent requests",
		   "allowed_tools":["venmo__login","venmo__show_sent_payment_requests"]}
		]}` + "\n```"

	steps, err := s.parsePlannerOutput(raw, "appworld_agent", true)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(steps))
	}
	if steps[0].AdditionalToolAllowlists["appworld"][0] != "phone__login" {
		t.Fatalf("allowlist not attached: %+v", steps[0].AdditionalToolAllowlists)
	}
}

// T3 regression: replan-injected steps must preserve AdditionalToolAllowlists.
// Before the fix, scheduler_surgery.go's Step construction omitted the field,
// silently dropping tool narrowing for replanned steps.
func TestSubmitDecision_ReplanPreservesToolAllowlists(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	allowlists := map[string][]string{
		"appworld": {"phone__login", "phone__search_contacts"},
	}
	steps := []schemas.StepInput{
		{ID: "r1", RoleID: "appworld_agent", Task: "replanned with tool narrowing",
			AdditionalToolAllowlists: allowlists},
	}
	payload, err := json.Marshal(map[string]any{"replacement_steps": steps})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	graph := fixtureGraph(t, string(payload))
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	if err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-1", "replan", ""); err != nil {
		t.Fatalf("replan should succeed, got error: %v", err)
	}

	s.Mu.RLock()
	g := s.graphs[graph.TaskID]
	s.Mu.RUnlock()

	st, ok := g.Steps["r1"]
	if !ok {
		t.Fatal("replacement step r1 not injected")
	}
	if got := st.AdditionalToolAllowlists["appworld"]; len(got) != 2 ||
		got[0] != "phone__login" || got[1] != "phone__search_contacts" {
		t.Fatalf("AdditionalToolAllowlists not preserved, got: %v", st.AdditionalToolAllowlists)
	}
}
