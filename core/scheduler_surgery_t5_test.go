package core

import (
	"encoding/json"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// T5 regression: when a replan injects replacement steps, any DependsOn
// reference to the deprecated step must be scrubbed. A deprecated step never
// reaches StepCompleted, so a replacement step that depends on it would
// silently deadlock — ReadySteps never returns it, the task hangs until
// timeout.
//
// Bug being pinned: mutateGraphTopologyLocked injected replacement steps with
// their DependsOn intact, including references to the step just deprecated.
// The dependency cleanup loop (lines 64-93) only scrubbed EXISTING graph
// steps, not the newly injected ones.

func TestReplan_ScrubsDeprecatedStepFromReplacementDeps(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	// Graph: s1 (completed) → s2 (failed, about to be deprecated via replan).
	graph := &schemas.TaskGraph{
		TaskID:        "task-t5-scrub",
		Steps:         make(map[string]*schemas.Step),
		Status:        schemas.GraphRunning,
		ContextTree:   map[string]*schemas.ContextNode{"root": {ID: "root", StepIDs: []string{"s1", "s2"}}},
		CurrentNodeID: "root",
	}
	graph.Steps["s1"] = &schemas.Step{
		ID:        "s1",
		RoleID:    "appworld_agent",
		Task:      "upstream step that succeeded",
		Status:    schemas.StepOK,
		DependsOn: nil,
	}
	graph.Steps["s2"] = &schemas.Step{
		ID:        "s2",
		RoleID:    "appworld_agent",
		Task:      "failing step to be deprecated",
		Status:    schemas.StepRunning,
		DependsOn: []string{"s1"},
	}
	graph.PendingDecisions = []*schemas.Decision{{
		ID:      "dec-t5",
		StepID:  "s2",
		Type:    schemas.DecisionAutonomousAbortRequested,
		Options: []string{"replan", "abort"},
	}}

	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	// Replacement steps: r1 mistakenly depends on s2 (the deprecated step).
	// r2 depends on r1 (valid — sibling replacement).
	replSteps := []schemas.StepInput{
		{ID: "r1", RoleID: "appworld_agent", Task: "replacement A", DependsOn: []string{"s2"}},
		{ID: "r2", RoleID: "appworld_agent", Task: "replacement B", DependsOn: []string{"r1"}},
	}
	payload, _ := json.Marshal(map[string]any{"replacement_steps": replSteps})

	if err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-t5", "replan", string(payload)); err != nil {
		t.Fatalf("replan should succeed, got: %v", err)
	}

	s.Mu.RLock()
	g := s.graphs[graph.TaskID]
	s.Mu.RUnlock()

	// s2 must be deprecated.
	if g.Steps["s2"].Status != schemas.StepDeprecated {
		t.Fatalf("s2 should be deprecated, got %s", g.Steps["s2"].Status)
	}

	// r1 must NOT depend on s2 — that reference must have been scrubbed.
	r1 := g.Steps["r1"]
	if r1 == nil {
		t.Fatal("replacement step r1 not injected")
	}
	for _, dep := range r1.DependsOn {
		if dep == "s2" {
			t.Fatalf("r1 still depends on deprecated step s2: %v — this causes a silent deadlock", r1.DependsOn)
		}
	}

	// r2's dependency on r1 is valid and must survive.
	r2 := g.Steps["r2"]
	if r2 == nil {
		t.Fatal("replacement step r2 not injected")
	}
	if !containsStr(r2.DependsOn, "r1") {
		t.Fatalf("r2 should still depend on r1: %v", r2.DependsOn)
	}
}

// A replacement step may legitimately depend on a surviving original step
// (e.g. s1 completed successfully). That reference must NOT be scrubbed.
func TestReplan_PreservesValidOriginalDeps(t *testing.T) {
	s, _ := newPlannerTestEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID:        "task-t5-preserve",
		Steps:         make(map[string]*schemas.Step),
		Status:        schemas.GraphRunning,
		ContextTree:   map[string]*schemas.ContextNode{"root": {ID: "root", StepIDs: []string{"s1", "s2"}}},
		CurrentNodeID: "root",
	}
	graph.Steps["s1"] = &schemas.Step{
		ID:     "s1",
		RoleID: "appworld_agent",
		Task:   "completed upstream",
		Status: schemas.StepOK,
	}
	graph.Steps["s2"] = &schemas.Step{
		ID:        "s2",
		RoleID:    "appworld_agent",
		Task:      "failing step",
		Status:    schemas.StepRunning,
		DependsOn: []string{"s1"},
	}
	graph.PendingDecisions = []*schemas.Decision{{
		ID:      "dec-t5b",
		StepID:  "s2",
		Type:    schemas.DecisionAutonomousAbortRequested,
		Options: []string{"replan", "abort"},
	}}

	s.Mu.Lock()
	s.graphs[graph.TaskID] = graph
	s.Mu.Unlock()

	// r1 depends on s1 (completed, NOT deprecated) — valid, must survive.
	replSteps := []schemas.StepInput{
		{ID: "r1", RoleID: "appworld_agent", Task: "replacement", DependsOn: []string{"s1"}},
	}
	payload, _ := json.Marshal(map[string]any{"replacement_steps": replSteps})

	if err := s.SubmitDecisionWithPayload(graph.TaskID, "dec-t5b", "replan", string(payload)); err != nil {
		t.Fatalf("replan should succeed, got: %v", err)
	}

	s.Mu.RLock()
	g := s.graphs[graph.TaskID]
	s.Mu.RUnlock()

	r1 := g.Steps["r1"]
	if r1 == nil {
		t.Fatal("r1 not injected")
	}
	if !containsStr(r1.DependsOn, "s1") {
		t.Fatalf("r1 should still depend on s1 (completed, not deprecated): %v", r1.DependsOn)
	}
}
