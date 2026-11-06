package core

import (
	"encoding/json"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// Register O1 regression: Case 2.5 of the tiered audit gate
// (core/scheduler_decision_audit.go) reads step.DeterministicChecks, but until
// 2026-11-04 nothing in the repository could ever write it — SOPStep and
// StepInput had no counterpart field, so a SOP file's "deterministic_checks"
// key was dropped at parse time and the same key in a POST /api/tasks/submit
// body was ignored by the decoder. Case 2.5 was therefore unreachable by
// declaration.
//
// These tests pin the whole declared channel, hop by hop:
// SOP JSON → SOPStep → StepInput → Step (fast path, DAG rebuild, replan
// surgery). They say nothing about what the gate then does with the checks;
// core/verify's own tests cover the verifiers.

const checksProbeSOP = `{
  "id": "checks_probe",
  "version": "1.0",
  "description": "probe SOP that declares deterministic checks",
  "triggers": [{"keywords": ["probe"]}],
  "steps": {
    "s1": {
      "id": "s1",
      "role": "engineer",
      "task": "write the report",
      "exit_criteria": "file:report.md",
      "deterministic_checks": [{"type": "file_exists", "params": {"path": "report.md"}}]
    }
  }
}`

func declaredChecks() []schemas.DeterministicCheck {
	return []schemas.DeterministicCheck{
		{Type: "file_exists", Params: map[string]any{"path": "report.md"}},
	}
}

// assertFileExistsCheck fails unless checks holds exactly one file_exists entry
// carrying the declared path parameter.
func assertFileExistsCheck(t *testing.T, where string, checks []schemas.DeterministicCheck) {
	t.Helper()
	if len(checks) != 1 {
		t.Fatalf("%s: want 1 declared check, got %d (%#v)", where, len(checks), checks)
	}
	if checks[0].Type != "file_exists" {
		t.Fatalf("%s: check type = %q, want file_exists", where, checks[0].Type)
	}
	if path, _ := checks[0].Params["path"].(string); path != "report.md" {
		t.Fatalf("%s: params lost, got %#v", where, checks[0].Params)
	}
}

// Hop 1+2: the SOP file's JSON must reach SOPStep, and expandSOP must carry it
// onto the StepInput it builds for the engine.
func TestDeterministicChecks_SurviveSOPParseAndExpansion(t *testing.T) {
	engine, _ := buildSmartRoutingEngine(t, false)
	defer engine.Stop()

	var sop schemas.SOP
	if err := json.Unmarshal([]byte(checksProbeSOP), &sop); err != nil {
		t.Fatalf("unmarshal probe SOP: %v", err)
	}
	assertFileExistsCheck(t, "SOPStep", sop.Steps["s1"].DeterministicChecks)

	engine.registry.Mu.Lock()
	engine.registry.SOPs[sop.ID] = &sop
	engine.registry.Mu.Unlock()

	gd := NewGroupDispatcher(engine, engine.registry)
	inputs, err := gd.expandSOP(&config.RoleGroup{
		ID:     "g_checks",
		Policy: config.GroupPolicySOP,
		SOPRef: sop.ID,
	}, "run the probe", nil)
	if err != nil {
		t.Fatalf("expandSOP: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("want 1 StepInput, got %d", len(inputs))
	}
	assertFileExistsCheck(t, "StepInput from SOP", inputs[0].DeterministicChecks)
}

// Hop 3a: a single step with no dependencies takes the inline fast path in
// SubmitWithSessionIR, which builds its own schemas.Step literal.
func TestDeterministicChecks_ReachGraphStepFastPath(t *testing.T) {
	engine, _ := buildSmartRoutingEngine(t, false)
	defer engine.Stop()

	taskID, err := engine.Submit([]schemas.StepInput{{
		ID:                  "s1",
		RoleID:              "engineer",
		Task:                "write report.md",
		DeterministicChecks: declaredChecks(),
	}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	graph := waitForGraph(t, engine, taskID, nil)
	if graph == nil {
		t.Fatal("fast-path task has no graph entry")
	}

	engine.Mu.RLock()
	step := graph.Steps["s1"]
	var checks []schemas.DeterministicCheck
	if step != nil {
		checks = append(checks, step.DeterministicChecks...)
	}
	engine.Mu.RUnlock()

	if step == nil {
		t.Fatal("step s1 missing from fast-path graph")
	}
	assertFileExistsCheck(t, "Step from fast path", checks)
}

// Hop 3b: a multi-step graph takes the DAG rebuild literal instead.
func TestDeterministicChecks_ReachGraphStepDAG(t *testing.T) {
	engine, _ := buildSmartRoutingEngine(t, false)
	defer engine.Stop()

	taskID, err := engine.Submit([]schemas.StepInput{
		{ID: "s1", RoleID: "engineer", Task: "draft report.md"},
		{ID: "s2", RoleID: "engineer", Task: "audit report.md",
			DependsOn: []string{"s1"}, DeterministicChecks: declaredChecks()},
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	graph := waitForGraph(t, engine, taskID, func(g *schemas.TaskGraph) bool {
		return len(g.Steps) >= 2
	})
	if graph == nil {
		t.Fatal("DAG task did not build 2 steps")
	}

	engine.Mu.RLock()
	step := graph.Steps["s2"]
	var checks []schemas.DeterministicCheck
	if step != nil {
		checks = append(checks, step.DeterministicChecks...)
	}
	engine.Mu.RUnlock()

	if step == nil {
		t.Fatal("step s2 missing from DAG graph")
	}
	assertFileExistsCheck(t, "Step from DAG rebuild", checks)
}

// Hop 4: a replan decision rebuilds steps through the surgery literal, so
// declared checks must survive a topology change too — otherwise the gate's
// input would be lost exactly when the plan is rewritten.
func TestDeterministicChecks_SurviveReplanSurgery(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	steps := []schemas.StepInput{
		{ID: "r1", RoleID: "appworld_agent", Task: "replanned part A",
			DeterministicChecks: declaredChecks()},
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
		t.Fatalf("replan: %v", err)
	}

	s.Mu.RLock()
	step := s.graphs[graph.TaskID].Steps["r1"]
	var checks []schemas.DeterministicCheck
	if step != nil {
		checks = append(checks, step.DeterministicChecks...)
	}
	s.Mu.RUnlock()

	if step == nil {
		t.Fatal("replanned step r1 not injected")
	}
	assertFileExistsCheck(t, "Step from replan surgery", checks)
}

// The decoder boundary: POST /api/tasks/submit hands the body straight to
// []schemas.StepInput, so the JSON key has to bind without a bespoke mapping.
func TestDeterministicChecks_BindFromSubmitRequestBody(t *testing.T) {
	var inputs []schemas.StepInput
	body := `[{"id":"s1","task":"t","deterministic_checks":[{"type":"command_pass","params":{"command":"go vet ./..."}}]}]`
	if err := json.Unmarshal([]byte(body), &inputs); err != nil {
		t.Fatalf("decode submit body: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("want 1 step, got %d", len(inputs))
	}
	if got := inputs[0].DeterministicChecks; len(got) != 1 || got[0].Type != "command_pass" {
		t.Fatalf("submit body key not bound: %#v", got)
	}
}
