package core

import (
	"strings"
	"testing"

	"github.com/daybeam/vortex/schemas"
)

// TestRepeatedToolFailure_ReturnsHighest verifies the E2/E3 helper that exposes
// the worst task-level tool failure count.
func TestRepeatedToolFailure_ReturnsHighest(t *testing.T) {
	s := &Spawner{}
	s.incrTaskToolFail("task-x", "tool_a", 1)
	s.incrTaskToolFail("task-x", "tool_a", 1) // 2
	s.incrTaskToolFail("task-x", "tool_b", 1)
	s.incrTaskToolFail("task-x", "tool_b", 1)
	s.incrTaskToolFail("task-x", "tool_b", 1) // 3 — highest

	tool, n := s.RepeatedToolFailure("task-x")
	if tool != "tool_b" || n != 3 {
		t.Fatalf("expected (tool_b, 3), got (%s, %d)", tool, n)
	}

	// Unknown task → ("", 0)
	if tool, n := s.RepeatedToolFailure("nonexistent"); tool != "" || n != 0 {
		t.Fatalf("expected empty for unknown task, got (%s, %d)", tool, n)
	}
}

// newFailEngine builds an engine whose spawner is usable for failure tests.
func newFailEngine(t *testing.T) *DirectedEngine {
	t.Helper()
	s, reg := newAbortTestEngine(t)
	reg.System.MaxSpawnDepth = 2 // allow the follow-up path to be reachable
	return s
}

// TestHandleStepFailure_RepeatedToolFailure_NoFollowUp is the E2/E3 regression
// test: a low-confidence (would-be generative_uncertainty) step failure must
// NOT spawn a same-role follow-up when a tool has repeatedly failed, because
// that follow-up would just repeat the same failing tool call. The failure is
// reclassified as deterministic_tool_refusal.
func TestHandleStepFailure_RepeatedToolFailure_NoFollowUp(t *testing.T) {
	s := newFailEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-e23-det",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning, RoleID: "r1", Task: "exchange a non-delivered order"}
	graph.Steps["s1"] = step
	s.Mu.Lock()
	s.graphs["task-e23-det"] = graph
	s.Mu.Unlock()

	// Simulate a tool that failed 3 times at the task level.
	for i := 0; i < toolFailCircuitBreakerThreshold; i++ {
		s.spawner.incrTaskToolFail("task-e23-det", "exchange_order", 1)
	}

	// Low confidence would normally → generative_uncertainty → follow-up spawn.
	result := &SpawnResult{Output: schemas.SubagentOutput{
		Status:     schemas.StatusPartial,
		Confidence: 0.3,
	}}

	s.handleStepFailure(graph, step, result)

	s.Mu.RLock()
	defer s.Mu.RUnlock()
	for id := range graph.Steps {
		if strings.Contains(id, "_spawn_") {
			t.Fatalf("follow-up substep %q should NOT be spawned on deterministic tool refusal", id)
		}
	}
	if len(graph.PendingDecisions) == 0 {
		t.Fatal("expected a decision to be raised")
	}
	rc, _ := graph.PendingDecisions[len(graph.PendingDecisions)-1].Context["root_cause"].(string)
	if rc != string(FailureClassDeterministicToolRefusal) {
		t.Fatalf("root_cause = %q, want %q", rc, FailureClassDeterministicToolRefusal)
	}
}

// TestHandleStepFailure_NoRepeatedFailure_SpawnsFollowUp is the control: with
// no repeated tool failure, a generative_uncertainty step failure still spawns
// a same-role follow-up (existing behavior preserved).
func TestHandleStepFailure_NoRepeatedFailure_SpawnsFollowUp(t *testing.T) {
	s := newFailEngine(t)
	defer s.Stop()

	graph := &schemas.TaskGraph{
		TaskID: "task-e23-gen",
		Steps:  make(map[string]*schemas.Step),
		Status: schemas.GraphRunning,
	}
	step := &schemas.Step{ID: "s1", Status: schemas.StepRunning, RoleID: "r1", Task: "explore an uncertain task"}
	graph.Steps["s1"] = step
	s.Mu.Lock()
	s.graphs["task-e23-gen"] = graph
	s.Mu.Unlock()

	result := &SpawnResult{Output: schemas.SubagentOutput{
		Status:     schemas.StatusPartial,
		Confidence: 0.3,
	}}

	s.handleStepFailure(graph, step, result)

	s.Mu.RLock()
	defer s.Mu.RUnlock()
	found := false
	for id := range graph.Steps {
		if strings.Contains(id, "_spawn_") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a follow-up substep when there is no repeated tool failure")
	}
}
