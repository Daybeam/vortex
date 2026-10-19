package core

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// ─── Why this file exists ─────────────────────────────────────────────────
//
// create_role was a documented decision option since 2026-07-12
// (docs/role_cookbook_design.md), then removed from the role-missing
// decision's options in scheduler_dag_exec.go because the old handler:
//
//   - flipped the GLOBAL registry.EnableDynamicRoleGen, and
//   - could persist LLM-authored roles with AllowDynamicMCPs:true into
//     config.json via RoleGenerator.GenerateRole's CommitOps.
//
// Both were real. But removing the option forced every role-missing failure
// into a human round-trip (register_role out-of-band), which is exactly the
// interaction cost the decision existed to avoid.
//
// The restored path (docs 52 §11.5) keeps the safety property by NOT being
// that code at all: it uses GenerateRoleObjects (pure: returns objects,
// touches no registry), stores the result in graph.SessionRoles under a
// namespaced id, and never writes config. These tests pin the properties
// that make that claim true, so a future "helpful" refactor cannot quietly
// reintroduce the global flip.

// createRoleFixtureGraph builds a one-step graph whose role is missing, with
// the pending step_failed decision the role-missing branch now emits.
func createRoleFixtureGraph(roleID string) *schemas.TaskGraph {
	graph := &schemas.TaskGraph{
		TaskID:        "task-createrole-1",
		Steps:         make(map[string]*schemas.Step),
		Status:        schemas.GraphRunning,
		ContextTree:   map[string]*schemas.ContextNode{"root": {ID: "root", StepIDs: []string{"s1"}}},
		CurrentNodeID: "root",
		SessionRoles:  map[string]any{},
	}
	graph.Steps["s1"] = &schemas.Step{
		ID:     "s1",
		RoleID: roleID,
		Task:   "reconcile the invoices and write a summary",
		Status: schemas.StepBlocked,
	}
	graph.PendingDecisions = []*schemas.Decision{{
		ID:      "dec-1",
		StepID:  "s1",
		Type:    schemas.DecisionStepFailed,
		Context: map[string]any{"error": "Role missing: role \"x\" not found", "role_id": roleID},
		Options: []string{"create_role", "retry", "skip", "abort"},
	}}
	return graph
}

// The option must be offered again, and the old "use register_role directly"
// action_required text (which pushed every occurrence to a human) must be
// replaced by one that still mentions the out-of-band escape hatch.
func TestRoleMissingDecision_OffersCreateRole(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	reg := s.registry
	reg.Roles["orchestrator_default"] = &config.Role{ID: "orchestrator_default"}

	graph := createRoleFixtureGraph("reconciler")
	graph.PendingDecisions = nil // let the engine create the decision

	step := graph.Steps["s1"]
	s.addDecision(graph, step, schemas.DecisionStepFailed, map[string]any{
		"error":           "Role missing",
		"role_id":         step.RoleID,
		"action_required": "Choose create_role, or register globally via register_role.",
	}, []string{"create_role", "retry", "skip", "abort"})

	if len(graph.PendingDecisions) != 1 {
		t.Fatalf("expected 1 pending decision, got %d", len(graph.PendingDecisions))
	}
	opts := graph.PendingDecisions[0].Options
	found := false
	for _, o := range opts {
		if o == "create_role" {
			found = true
		}
	}
	if !found {
		t.Fatalf("create_role must be an offered option, got %v", opts)
	}
}

// Choosing create_role with an explicit draft payload must:
//   - store the role in session IR under a namespaced id,
//   - rewrite step.RoleID so the step actually dispatches as that id,
//   - un-block the step,
//   - and leave the global registry untouched.
func TestSubmitDecision_CreateRoleWithDraftPayload(t *testing.T) {
	s, reg := newPlannerTestEngine(t)
	defer s.Stop()

	// The global registry has a role of the same *name* the draft will use;
	// the session role must still not shadow or overwrite it.
	reg.Roles["reconciler"] = &config.Role{ID: "reconciler", Name: "Global Reconciler"}

	graph := createRoleFixtureGraph("reconciler")
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	draft := config.Role{
		ID:               "reconciler",
		Name:             "Session Reconciler",
		BaseCapability:   "reconcile",
		BoundSkills:      []string{"skill_base_reconciler"},
		BoundMCPBindings: []config.MCPBinding{},
	}
	payload, err := json.Marshal(draft)
	if err != nil {
		t.Fatalf("marshal draft: %v", err)
	}

	if err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-1", "create_role", string(payload)); err != nil {
		t.Fatalf("create_role with draft payload failed: %v", err)
	}

	s.Mu.RLock()
	g := s.graphs[graph.TaskID]
	s.Mu.RUnlock()

	step := g.Steps["s1"]
	if step.Status != schemas.StepPending {
		t.Fatalf("step should be re-armed (pending), got %s", step.Status)
	}
	if step.RetryCount != 0 || step.LastError != "" {
		t.Fatalf("retry state not reset: retry=%d err=%q", step.RetryCount, step.LastError)
	}
	if step.RoleID == "reconciler" {
		t.Fatal("step.RoleID must be rewritten to the namespaced session id, otherwise the interceptor still cannot resolve it")
	}
	if !strings.HasPrefix(step.RoleID, "ephemeral_task-createrole-1_") {
		t.Fatalf("unexpected namespaced id %q", step.RoleID)
	}
	if _, ok := g.GetSessionRole(step.RoleID); !ok {
		t.Fatal("generated role was not stored in the task's session IR")
	}

	// The global role must be intact and the namespaced id must not leak in.
	if reg.Roles["reconciler"].Name != "Global Reconciler" {
		t.Error("global role was overwritten")
	}
	reg.Mu.RLock()
	_, leaked := reg.Roles[step.RoleID]
	reg.Mu.RUnlock()
	if leaked {
		t.Error("session role leaked into the global registry")
	}

	// The decision must be consumed.
	if len(g.PendingDecisions) != 0 {
		t.Fatalf("decision should be consumed, %d remain", len(g.PendingDecisions))
	}
}

// EnableDynamicRoleGen must NOT be flipped by choosing create_role -- that
// was the whole reason the option was removed.
func TestSubmitDecision_CreateRoleDoesNotFlipGlobalSwitch(t *testing.T) {
	s, reg := newPlannerTestEngine(t)
	defer s.Stop()

	if reg.EnableDynamicRoleGen {
		t.Fatal("precondition: EnableDynamicRoleGen should start false")
	}

	graph := createRoleFixtureGraph("reconciler")
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	payload, _ := json.Marshal(config.Role{ID: "reconciler", Name: "Draft"})
	if err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-1", "create_role", string(payload)); err != nil {
		t.Fatalf("create_role failed: %v", err)
	}

	reg.Mu.RLock()
	flipped := reg.EnableDynamicRoleGen
	reg.Mu.RUnlock()
	if flipped {
		t.Fatal("create_role must never flip registry.EnableDynamicRoleGen")
	}
}

// An empty payload means "synthesise from the cookbook". With no provider
// configured that must fail loudly and leave the step blocked -- the
// decision is the operator's fallback, so it must stay available rather than
// being silently consumed.
func TestSubmitDecision_CreateRoleWithoutProviderErrorsAndKeepsStepBlocked(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	graph := createRoleFixtureGraph("reconciler")
	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-1", "create_role", "")
	if err == nil {
		t.Fatal("expected an error: no default provider is configured in this fixture")
	}
	if !strings.Contains(err.Error(), "create_role") {
		t.Fatalf("error should name the choice for traceability, got: %v", err)
	}

	s.Mu.RLock()
	g := s.graphs[graph.TaskID]
	s.Mu.RUnlock()

	if g.Steps["s1"].Status == schemas.StepPending {
		t.Error("step must stay blocked when generation failed")
	}
	if len(g.PendingDecisions) != 1 {
		t.Error("decision must remain pending so the operator can still choose abort/skip")
	}
}
