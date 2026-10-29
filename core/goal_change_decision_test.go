package core

import (
	"testing"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// setupTestSchedulerWithGraph creates a minimal DirectedEngine with a single
// test graph for decision submission tests. Returns a cleanup function that
// must be deferred.
func setupTestSchedulerWithGraph(t *testing.T) (*DirectedEngine, *schemas.TaskGraph, string, func()) {
	t.Helper()
	taskID := "test_goal_change_task"
	graph := &schemas.TaskGraph{
		TaskID:    taskID,
		Status:    "running",
		Steps:     map[string]*schemas.Step{},
		CreatedAt: parseTestTime("2026-10-29T00:00:00Z"),
	}
	s := &DirectedEngine{
		graphs:     map[string]*schemas.TaskGraph{taskID: graph},
		doneChans:  map[string]chan struct{}{taskID: make(chan struct{})},
		notifyChan: make(chan struct{}),
	}
	// Initialize a real logger so s.logger.Log doesn't panic.
	logger, _ := NewLogger(t.TempDir(), nil)
	s.logger = logger
	cleanup := func() { logger.Close() }
	return s, graph, taskID, cleanup
}

func parseTestTime(s string) (t time.Time) {
	t, _ = time.Parse(time.RFC3339, s)
	return
}

// TestGoalChangeDecision_Type verifies that the goal_change_requested decision
// type exists and is correctly typed.
//
// See docs/plan/TASK_DRIFT_CONTROL_DESIGN.md §P2.
func TestGoalChangeDecision_Type(t *testing.T) {
	if schemas.DecisionGoalChangeRequested != "goal_change_requested" {
		t.Errorf("DecisionGoalChangeRequested = %q, want %q",
			schemas.DecisionGoalChangeRequested, "goal_change_requested")
	}
}

// TestGoalChangeDecision_AdminOnly verifies that goal_change_requested is NOT
// in the public tier option set — only admin tier can approve goal changes.
func TestGoalChangeDecision_AdminOnly(t *testing.T) {
	if PublicTierAllowsChoice("goal_change_requested") {
		t.Error("goal_change_requested should be admin-only, but PublicTierAllowsChoice returned true")
	}
}

// TestGoalChangeDecision_CharterVersionIncrement verifies that submitting a
// goal_change_requested decision increments the charter version and updates
// the graph's charter.
func TestGoalChangeDecision_CharterVersionIncrement(t *testing.T) {
	s, graph, taskID, cleanup := setupTestSchedulerWithGraph(t)
	defer cleanup()

	// Set an initial charter
	graph.Charter = &schemas.TaskCharter{
		Intent:  "Original intent",
		Version: 1,
	}

	// Create a pending decision
	dec := &schemas.Decision{
		ID:     "dec_goal_change",
		StepID: "s1",
		Type:   schemas.DecisionGoalChangeRequested,
	}
	graph.PendingDecisions = append(graph.PendingDecisions, dec)
	graph.Steps["s1"] = &schemas.Step{ID: "s1", Status: schemas.StepBlocked}

	// Submit goal_change_requested with a new charter
	newCharterJSON := `{"intent":"New intent after pivot","non_goals":["do not do X"],"constraints":["must keep Y"]}`
	err := s.SubmitDecisionWithPayload(taskID, "dec_goal_change", "goal_change_requested", newCharterJSON)
	if err != nil {
		t.Fatalf("SubmitDecisionWithPayload failed: %v", err)
	}

	// Verify charter was updated
	if graph.Charter == nil {
		t.Fatal("charter is nil after goal_change_requested")
	}
	if graph.Charter.Intent != "New intent after pivot" {
		t.Errorf("charter.Intent = %q, want %q", graph.Charter.Intent, "New intent after pivot")
	}
	if graph.Charter.Version != 2 {
		t.Errorf("charter.Version = %d, want 2 (incremented from 1)", graph.Charter.Version)
	}
	if len(graph.Charter.NonGoals) != 1 || graph.Charter.NonGoals[0] != "do not do X" {
		t.Errorf("charter.NonGoals = %v, want [\"do not do X\"]", graph.Charter.NonGoals)
	}
	if len(graph.Charter.Constraints) != 1 || graph.Charter.Constraints[0] != "must keep Y" {
		t.Errorf("charter.Constraints = %v, want [\"must keep Y\"]", graph.Charter.Constraints)
	}
}

// TestGoalChangeDecision_FirstCharterVersionIsOne verifies that submitting
// goal_change_requested on a task with no prior charter sets Version to 1.
func TestGoalChangeDecision_FirstCharterVersionIsOne(t *testing.T) {
	s, graph, taskID, cleanup := setupTestSchedulerWithGraph(t)
	defer cleanup()

	// No initial charter
	if graph.Charter != nil {
		t.Fatal("expected nil charter initially")
	}

	dec := &schemas.Decision{
		ID:     "dec_goal_change_2",
		StepID: "s1",
		Type:   schemas.DecisionGoalChangeRequested,
	}
	graph.PendingDecisions = append(graph.PendingDecisions, dec)
	graph.Steps["s1"] = &schemas.Step{ID: "s1", Status: schemas.StepBlocked}

	newCharterJSON := `{"intent":"First charter"}`
	err := s.SubmitDecisionWithPayload(taskID, "dec_goal_change_2", "goal_change_requested", newCharterJSON)
	if err != nil {
		t.Fatalf("SubmitDecisionWithPayload failed: %v", err)
	}

	if graph.Charter == nil {
		t.Fatal("charter is nil after goal_change_requested")
	}
	if graph.Charter.Version != 1 {
		t.Errorf("charter.Version = %d, want 1 (first charter)", graph.Charter.Version)
	}
}
